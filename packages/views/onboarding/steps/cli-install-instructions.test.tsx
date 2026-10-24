import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enOnboarding from "../../locales/en/onboarding.json";
import { CliInstallInstructions } from "./cli-install-instructions";

const TEST_RESOURCES = { en: { common: enCommon, onboarding: enOnboarding } };

const BUILD_CMD =
  "cd server && go build -o /usr/local/bin/multica ./cmd/multica";

describe("CliInstallInstructions", () => {
  // The switch itself is covered in common/cli-install-command.test.tsx; this
  // checks the card wires it into step 1 and leaves step 2 shared.
  it("offers the build command in step 1 and keeps setup shared", () => {
    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <CliInstallInstructions />
      </I18nProvider>,
    );

    expect(screen.getByText(BUILD_CMD)).toBeInTheDocument();
    expect(screen.getByText("multica setup")).toBeInTheDocument();
  });
});
