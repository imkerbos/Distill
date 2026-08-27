package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	applog "github.com/imkerbos/Distill/internal/log"
)

// fakeUI 是一份最小的前端产物。
func fakeUI() WebUI {
	return fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><title>distill</title>")},
		"assets/app.js": {Data: []byte("console.log(1)")},
		"favicon.svg":   {Data: []byte("<svg/>")},
	}
}

func getUI(t *testing.T, ui WebUI, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mountWebUI(ui)(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// 根路径要给出前端首页。
//
// 这条用例来自一次真实部署：生产镜像里根本没有前端，后端也没有任何服务
// 静态文件的路由，打开首页拿到的是一句 JSON 的「请求的资源不存在」。
// 本机开发时前端跑在独立的 dev server 上，所以这条路径从来没被走到过。
func TestWebUIServesTheIndexAtRoot(t *testing.T) {
	rec := getUI(t, fakeUI(), http.MethodGet, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<!doctype html") {
		t.Errorf("body = %q, want the index document", rec.Body.String())
	}
}

// 单页应用的路由交给浏览器：产物里没有 /clusters 这个文件，但它是一个合法地址。
func TestWebUIFallsBackToTheIndexForAppRoutes(t *testing.T) {
	rec := getUI(t, fakeUI(), http.MethodGet, "/clusters")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /clusters = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<!doctype html") {
		t.Error("单页应用的路径没有回到首页，浏览器侧的路由接不上")
	}
}

// 但静态资源找不到就是找不到。
//
// 回 index.html 会让浏览器把一份 HTML 当 JavaScript 解析，报出的错与真正的
// 成因毫无关系 —— 一个拼错的资源路径会表现成"语法错误"。
func TestWebUIDoesNotFallBackForMissingAssets(t *testing.T) {
	for _, p := range []string{"/assets/missing.js", "/nope.css", "/x.png"} {
		rec := getUI(t, fakeUI(), http.MethodGet, p)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 —— 带扩展名的路径回首页会把成因藏起来", p, rec.Code)
		}
	}
}

// 存在的静态资源照常给出。
func TestWebUIServesExistingAssets(t *testing.T) {
	rec := getUI(t, fakeUI(), http.MethodGet, "/assets/app.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /assets/app.js = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "console.log(1)" {
		t.Errorf("body = %q, want the asset itself", rec.Body.String())
	}
}

// **API 路径永远拿 JSON 的 404，不拿首页。**
//
// 一条写错的 API 路径回一份 index.html，调用方会以为自己拿到了数据，
// 直到 JSON 解析失败 —— 而那个错误指向的位置与真正的成因毫无关系。
func TestWebUINeverAnswersAPIPaths(t *testing.T) {
	for _, p := range []string{apiPrefix + "/nope", apiPrefix + "/clusters/x/typo"} {
		rec := getUI(t, fakeUI(), http.MethodGet, p)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "<!doctype html") {
			t.Errorf("GET %s 回了首页；调用方会把它当成数据", p)
		}
	}
}

// 非 GET/HEAD 不走前端：一个 POST 到未知路径是调用方用错了接口。
func TestWebUIOnlyAnswersReads(t *testing.T) {
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := getUI(t, fakeUI(), m, "/clusters")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s /clusters = %d, want 404", m, rec.Code)
		}
	}
}

// 带了前端产物的部署，根路径必须真的给出首页。
//
// 上面那几条都直接调 mountWebUI，绕过了「路由到底有没有把它挂上去」这一环 ——
// 实测把 NewRouter 里那一行摘掉，它们照样全绿。这条走真路由。
func TestRouterMountsTheWebUIAtRoot(t *testing.T) {
	logger, err := applog.New("ERROR", io.Discard)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	r := NewRouter(Deps{Logger: logger, WebUI: fakeUI()})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200 —— 前端产物在，但路由没有把它挂上去", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<!doctype html") {
		t.Errorf("body = %q, want the index document", rec.Body.String())
	}
}

// 不带前端的部署照旧回 JSON 的 404，与加这一层之前完全一样。
//
// 走真路由而不是判断一个 nil：本机开发就是这种形态，它不该因为多了这一层
// 而改变行为。
func TestRouterWithoutWebUIKeepsThePlainNotFound(t *testing.T) {
	logger, err := applog.New("ERROR", io.Discard)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	r := NewRouter(Deps{Logger: logger})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET / = %d, want 404 when no web UI is bundled", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<!doctype html") {
		t.Error("没有前端产物的部署却给出了 HTML")
	}
}
