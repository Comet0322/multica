package main

import (
	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/handler"
)

// registerExtRoutes mounts the fork-only /api/ext/* routes. They live in this
// file so router.go needs a single hook line, and an upstream merge of
// router.go never conflicts with them. Call it inside the workspace-scoped,
// authenticated route group.
//
// Template writes, run cancel and step decisions are human-only: an agent's
// task token authenticates as its runtime owner, and §6.3 lets agents decide
// only through the comment protocol, never with the owner's permissions.
func registerExtRoutes(r chi.Router, h *handler.Handler) {
	r.Route("/api/ext/workflows", func(r chi.Router) {
		r.Get("/", h.ListExtWorkflows)
		r.With(handler.RequireHumanActor).Post("/", h.CreateExtWorkflow)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", h.GetExtWorkflow)
			r.With(handler.RequireHumanActor).Put("/", h.UpdateExtWorkflow)
			r.With(handler.RequireHumanActor).Delete("/", h.DeleteExtWorkflow)
			r.Get("/runs", h.ListExtWorkflowRunsForWorkflow)
		})
	})
	r.Route("/api/ext/workflow-runs", func(r chi.Router) {
		r.Get("/", h.ListExtWorkflowRunsForIssue)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", h.GetExtWorkflowRun)
			r.With(handler.RequireHumanActor).Post("/cancel", h.CancelExtWorkflowRun)
			r.With(handler.RequireHumanActor).Post("/steps/{stepId}/decision", h.DecideExtWorkflowStep)
		})
	})
}
