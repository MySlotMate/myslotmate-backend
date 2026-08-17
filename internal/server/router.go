package server

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"myslotmate-backend/internal/controller"
	"myslotmate-backend/internal/firebase"
	"myslotmate-backend/internal/lib/realtime"
)

const swaggerHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8"/>
  <title>MySlotMate API Docs</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css"/>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    SwaggerUIBundle({
      url:"/swagger.yaml",
      dom_id:"#swagger-ui",
      presets:[SwaggerUIBundle.presets.apis,SwaggerUIBundle.SwaggerUIStandalonePreset],
      layout:"BaseLayout"
    });
  </script>
</body>
</html>`

// NewRouter builds the HTTP router with all routes and middleware wired.
func NewRouter(
	fbApp *firebase.App,
	socketService *realtime.SocketService,
	userCtrl *controller.UserController,
	hostCtrl *controller.HostController,
	eventCtrl *controller.EventController,
	bookingCtrl *controller.BookingController,
	couponCtrl *controller.CouponController,
	joinRequestCtrl *controller.JoinRequestController,
	walkInCtrl *controller.WalkInController,
	bookingImportCtrl *controller.BookingImportController,
	reviewCtrl *controller.ReviewController,
	inboxCtrl *controller.InboxController,
	payoutCtrl *controller.PayoutController,
	webhookCtrl *controller.WebhookController,
	supportCtrl *controller.SupportController,
	uploadCtrl *controller.UploadController,
	adminCtrl *controller.AdminController,
	adminAuthCtrl *controller.AdminAuthController,
	adminDirectoryCtrl *controller.AdminDirectoryController,
	adminDashboardCtrl *controller.AdminDashboardController,
	blogCtrl *controller.BlogController,
	experienceTemplateCtrl *controller.ExperienceTemplateController,
	ragChatCtrl *controller.RAGChatController,
	ragDocCtrl *controller.RAGDocumentController,
) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(corsMiddlewareAllowAll())

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Server-side image proxy so browser clients (e.g. the admin ticket
	// generator) can embed remote images without hitting CORS. Restricted to
	// https + an allowlist of hosts to avoid SSRF.
	r.Get("/image-proxy", imageProxyHandler)

	// Swagger UI & spec
	r.Get("/docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(swaggerHTML))
	})
	r.Get("/swagger.yaml", func(w http.ResponseWriter, r *http.Request) {
		data, err := os.ReadFile("swagger.yaml")
		if err != nil {
			http.Error(w, "swagger.yaml not found", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
		_, _ = w.Write(data)
	})

	if socketService != nil {
		r.Handle("/socket.io/*", socketService.GetServer())
	}

	if userCtrl != nil {
		userCtrl.RegisterRoutes(r)
	}

	if hostCtrl != nil {
		hostCtrl.RegisterRoutes(r)
	}

	if eventCtrl != nil {
		eventCtrl.RegisterRoutes(r)
	}

	if couponCtrl != nil {
		couponCtrl.RegisterRoutes(r)
	}

	if joinRequestCtrl != nil {
		joinRequestCtrl.RegisterRoutes(r)
		joinRequestCtrl.RegisterAdminRoutes(r)
	}

	if bookingCtrl != nil {
		bookingCtrl.RegisterRoutes(r)
	}

	if walkInCtrl != nil {
		walkInCtrl.RegisterRoutes(r)
	}

	if bookingImportCtrl != nil {
		bookingImportCtrl.RegisterRoutes(r)
	}

	if reviewCtrl != nil {
		reviewCtrl.RegisterRoutes(r)
	}

	if inboxCtrl != nil {
		inboxCtrl.RegisterRoutes(r)
	}

	if payoutCtrl != nil {
		payoutCtrl.RegisterRoutes(r)
	}

	if webhookCtrl != nil {
		webhookCtrl.RegisterRoutes(r)
	}

	if supportCtrl != nil {
		supportCtrl.RegisterRoutes(r)
	}

	if uploadCtrl != nil {
		uploadCtrl.RegisterRoutes(r)
	}

	if adminCtrl != nil {
		adminCtrl.RegisterRoutes(r)
	}

	if adminAuthCtrl != nil {
		adminAuthCtrl.RegisterRoutes(r)
	}

	if adminDirectoryCtrl != nil {
		adminDirectoryCtrl.RegisterRoutes(r)
	}

	if adminDashboardCtrl != nil {
		adminDashboardCtrl.RegisterRoutes(r)
	}

	if blogCtrl != nil {
		blogCtrl.RegisterRoutes(r)
	}

	if experienceTemplateCtrl != nil {
		experienceTemplateCtrl.RegisterRoutes(r)
	}

	if ragChatCtrl != nil {
		// RAG Chatbot routes
		r.Route("/api/chat/rag", func(r chi.Router) {
			r.Post("/", ragChatCtrl.Chat)
			r.Post("/clear", ragChatCtrl.ClearChat)
			r.Post("/delete", ragChatCtrl.DeleteChat)
		})
		r.Post("/api/admin/rag/ingest", ragChatCtrl.IngestData)
	}

	// RAG Document routes (added after ragChatCtrl check)
	if ragDocCtrl != nil {
		r.Post("/api/upload/rag-document", ragDocCtrl.UploadDocument)
		r.Get("/api/admin/rag/documents", ragDocCtrl.ListDocuments)
		r.Delete("/api/admin/rag/documents/{id}", ragDocCtrl.DeleteDocument)
	}

	return r
}

// imageProxyHandler fetches an allowed remote image server-side and streams it
// back, so browser clients can embed it (e.g. in a generated PDF) without CORS
// problems. Limited to https + an allowlist of hosts to avoid SSRF.
func imageProxyHandler(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		http.Error(w, "invalid url", http.StatusBadRequest)
		return
	}
	host := strings.ToLower(u.Hostname())
	if host != "api.qrserver.com" && !strings.HasSuffix(host, ".amazonaws.com") {
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, raw, nil)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "image/") {
		http.Error(w, "not an image", http.StatusUnsupportedMediaType)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = io.Copy(w, resp.Body)
}

// corsMiddlewareAllowAll enables CORS for all origins.
func corsMiddlewareAllowAll() func(http.Handler) http.Handler {
	const allowedMethods = "GET,POST,PUT,PATCH,DELETE,OPTIONS"
	const defaultAllowedHeaders = "Accept,Authorization,Content-Type,Origin,X-Requested-With"
	const exposedHeaders = "Content-Length,Content-Type"

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			if origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", "*")

				w.Header().Set("Access-Control-Allow-Methods", allowedMethods)

				requestHeaders := r.Header.Get("Access-Control-Request-Headers")
				if requestHeaders != "" {
					w.Header().Set("Access-Control-Allow-Headers", requestHeaders)
				} else {
					w.Header().Set("Access-Control-Allow-Headers", defaultAllowedHeaders)
				}

				w.Header().Set("Access-Control-Expose-Headers", exposedHeaders)
				w.Header().Set("Access-Control-Max-Age", "600")
			}

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
