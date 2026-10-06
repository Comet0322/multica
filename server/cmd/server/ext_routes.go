package main

import (
	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/handler"
)

// registerExtRoutes mounts the fork-only /api/ext/* routes. They live in this
// file so router.go needs a single hook line, and an upstream merge of
// router.go never conflicts with them. Call it inside the workspace-scoped,
// authenticated route group.
func registerExtRoutes(r chi.Router, h *handler.Handler) {
	r.Route("/api/ext/workflows", func(r chi.Router) {
		r.Get("/", h.ListExtWorkflows)
		r.Post("/", h.CreateExtWorkflow)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", h.GetExtWorkflow)
			r.Put("/", h.UpdateExtWorkflow)
			r.Delete("/", h.DeleteExtWorkflow)
			r.Get("/runs", h.ListExtWorkflowRunsForWorkflow)
		})
	})
	r.Route("/api/ext/workflow-runs", func(r chi.Router) {
		r.Get("/", h.ListExtWorkflowRunsForIssue)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", h.GetExtWorkflowRun)
			r.Post("/cancel", h.CancelExtWorkflowRun)
			r.Post("/steps/{stepId}/decision", h.DecideExtWorkflowStep)
		})
	})
}
