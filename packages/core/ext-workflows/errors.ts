import type { ExtWorkflowValidationError } from "./types";

/**
 * Thrown by `ApiClient.updateExtWorkflow` when the server answers 422
 * `{error: "validation_failed", errors: [...]}`. The editor maps `errors` onto
 * rows and fields instead of toasting one sentence.
 */
export class ExtWorkflowValidationFailed extends Error {
  readonly errors: ExtWorkflowValidationError[];

  constructor(errors: ExtWorkflowValidationError[]) {
    super(
      errors.length === 1 && errors[0]
        ? errors[0].message
        : `Workflow definition is invalid (${errors.length} problems)`,
    );
    this.name = "ExtWorkflowValidationFailed";
    this.errors = errors;
  }
}
