package httpapi

import (
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/imkerbos/Distill/internal/response"
)

// RateLimiter 是按来源地址计数的固定窗口限流器（规范 §23）。
//
// **计数只存在于本进程内，不跨实例。** 部署形态是 Cloud Run 上的无状态
// 多实例，因此 N 个实例上就有 N 份互相看不见的计数：实际生效的上限是
// N × limit，不是 limit；实例扩缩容还会让计数直接归零。这不是可以含糊
// 过去的实现细节，而是本方案已知的能力边界，写在这里而不是写在某份
// 评估文档里。平台没有 Redis，为这一个功能引入一个共享存储不属于最小
// 改动 —— 需要跨实例的真实上限时，替换的是 RateLimiter 的存储，
// 而不是它的调用点。
//
// 可用性代价是真实的，见 clientKey 与 Allow 上的说明：限流本身可以被
// 用来挡住正常用户。两个方向都在一个窗口内自愈。
type RateLimiter struct {
	limit   int
	window  time.Duration
	maxKeys int
	clock   func() time.Time

	mu      sync.Mutex
	windows map[string]*rateWindow
}

// rateWindow 是单个来源当前窗口内的计数。
type rateWindow struct {
	count   int
	resetAt time.Time
}

// NewRateLimiter 构造一个限流器：每 window 时间内每个来源至多 limit 次。
//
// clock 为 nil 时取 time.Now，与 auth.NewSessionStore 同一约定，
// 测试因此可以推进时间而不必真的等。
//
// maxKeys 是内存上界：单条计数是常数大小，键数封顶后整张表的占用也就
// 封顶了。攻击者换着来源打，最多把表撑到 maxKeys，撑不下去 —— 撑满之后
// 的处置见 Allow。
func NewRateLimiter(limit int, window time.Duration, maxKeys int, clock func() time.Time) *RateLimiter {
	if clock == nil {
		clock = time.Now
	}
	return &RateLimiter{
		limit:   limit,
		window:  window,
		maxKeys: maxKeys,
		clock:   clock,
		windows: make(map[string]*rateWindow),
	}
}

// Allow 记一次来自 key 的请求，并返回它是否被放行。
//
// 表撑满且回收不出空位时返回 false（拒绝），不是 true（放行）：放行等于
// 把「表满」这个可以由攻击者主动制造的条件，变成一个关掉限流的开关。
// 代价写在这里：一个用大量来源把表撑满的攻击者，能在一个窗口内连带挡住
// 尚未登记的正常用户。这个方向是刻意选的 —— 撞库必须挡住，登录短暂不可用
// 可以在一个窗口后自愈。
func (l *RateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clock()
	if w, ok := l.windows[key]; ok {
		if now.Before(w.resetAt) {
			if w.count >= l.limit {
				return false
			}
			w.count++
			return true
		}
		// 窗口已过，就地重置。已有的键不占新槽位，因此这条路径
		// 永远不会让表变大。
		w.count, w.resetAt = 1, now.Add(l.window)
		return true
	}

	if len(l.windows) >= l.maxKeys {
		l.dropExpired(now)
		if len(l.windows) >= l.maxKeys {
			return false
		}
	}
	l.windows[key] = &rateWindow{count: 1, resetAt: now.Add(l.window)}
	return true
}

// dropExpired 回收窗口已过的键。
//
// 只在表满时调用，不起后台 goroutine：一次 O(maxKeys) 的遍历被摊到
// 「表满」这个罕见事件上，而一个常驻的清理 goroutine 会成为每个测试、
// 每次装配都要负责关掉的东西。
func (l *RateLimiter) dropExpired(now time.Time) {
	for k, w := range l.windows {
		if !now.Before(w.resetAt) {
			delete(l.windows, k)
		}
	}
}

// Middleware 把限流挂进一条 handler 链。
//
// 拒绝时 handler 完全不执行 —— 对登录来说，这正是重点：那次 bcrypt
// 根本不会被算。
func (l *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow(clientKey(r)) {
			// 真实的 429 而不是 200 + code：限流是协议层的拒绝，
			// 网关与前端的重试逻辑都按状态码判断（与 RequireSession
			// 用真 401 同一条判据）。
			response.WriteSystem(w, http.StatusTooManyRequests, response.CodeRateLimited)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientKey 取限流的计数键。
//
// 只用 r.RemoteAddr，**绝不读 X-Forwarded-For**：那个头由调用方提供，
// 认它等于让攻击者每次请求换一个键 —— 限流当场归零，还能顺手把 maxKeys
// 撑满去连累别人。
//
// 代价是本服务跑在反向代理后面时（Cloud Run，以及本地的 vite proxy），
// RemoteAddr 是代理的地址，所有调用方共用同一个计数，上限退化成全局的。
// 这个方向是安全的（永远不会因为伪造而放宽），但它意味着一个攻击者能
// 吃光全局的登录预算，把正常用户一起挡在外面一个窗口。要摆脱这一点，
// 需要的是一份可信代理配置 + 从 XFF 里按跳数取真实来源，那是一个独立的
// 安全决定，不是这次改动的一部分。
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
