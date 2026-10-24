import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { CliSection } from "./cli-section";

vi.mock("../../i18n", () => ({
  useLocale: () => ({
    t: {
      download: {
        cli: {
          title: "Prefer the CLI?",
          sub: "For servers and headless setups.",
          installLabel: "Install",
          platformGroup: "Choose your platform",
          platformMacosLinux: "macOS / Linux",
          platformWindows: "Windows",
          startLabel: "Start daemon",
          sshNote: "Already on a server?",
          copyLabel: "Copy",
          copiedLabel: "Copied",
        },
      },
    },
  }),
}));

const BUILD_CMD =
  "cd server && go build -o /usr/local/bin/multica ./cmd/multica";

describe("CliSection", () => {
  // The switch itself is covered in @multica/views; this checks the landing
  // dictionary is wired into it and the daemon block stays shared.
  it("wires the platform switch into the install block", () => {
    render(<CliSection />);

    expect(screen.getByText(BUILD_CMD)).toBeInTheDocument();
    expect(screen.getByText("multica setup")).toBeInTheDocument();
  });
});
