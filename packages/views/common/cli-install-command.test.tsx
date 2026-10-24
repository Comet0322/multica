import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import {
  CLI_INSTALL_COMMANDS,
  CliInstallCommand,
} from "./cli-install-command";

const LABELS = {
  group: "Choose your platform",
  macosLinux: "macOS / Linux",
  windows: "Windows",
};

describe("CliInstallCommand", () => {
  it("offers the build command for macOS / Linux and no Windows tab", () => {
    render(
      <CliInstallCommand labels={LABELS}>
        {(command) => <code>{command}</code>}
      </CliInstallCommand>,
    );

    expect(
      screen.getByRole("tablist", { name: LABELS.group }),
    ).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "macOS / Linux" })).toHaveAttribute(
      "aria-selected",
      "true",
    );
    expect(screen.queryByRole("tab", { name: "Windows" })).toBeNull();
    expect(
      screen.getByText(CLI_INSTALL_COMMANDS.macosLinux),
    ).toBeInTheDocument();
  });
});
