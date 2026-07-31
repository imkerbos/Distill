package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/auth"
	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/store"
)

// Deps 是路由所需的全部依赖。
//
// 显式结构体而非全局变量：测试可以只装配自己需要的部分，
// 而不必让整个进程处于某种状态。
type Deps struct {
	// Sessions 是会话存储。
	Sessions *auth.SessionStore
	// Verifier 校验登录凭证。
	Verifier *auth.Verifier
	// Logger 是结构化日志器。
	Logger *slog.Logger
	// Reader 提供 Fleet 数据查询。
	Reader store.Reader
}

// NewRouter 装配 HTTP 路由。
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(RequestID)
	r.Use(RequestLogger(d.Logger))

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		response.WriteSystem(w, http.StatusNotFound, response.CodeNotFound)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		response.WriteSystem(w, http.StatusMethodNotAllowed, response.CodeInvalidParam)
	})

	r.Route("/api/v1", func(api chi.Router) {
		// 登录是唯一无需会话的端点。
		api.Post("/sessions", handleCreateSession(d))

		api.Group(func(protected chi.Router) {
			protected.Use(RequireSession(d.Sessions))
			protected.Delete("/sessions/current", handleDeleteSession(d))
			protected.Get("/sessions/current", handleCurrentSession())
			protected.Get("/clusters", handleListClusters(d))
			protected.Get("/clusters/{clusterID}/topology", handleTopology(d))
			protected.Get("/clusters/{clusterID}/quality", handleQuality(d))
			protected.Get("/flows", handleListFlows(d))
			protected.Get("/flows/{flowID}/decision", handleFlowDecision(d))
		})
	})

	return r
}
