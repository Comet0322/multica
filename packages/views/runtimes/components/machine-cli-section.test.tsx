// @vitest-environment jsdom

import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enRuntimes from "../../locales/en/runtimes.json";
import type { RuntimeMachine } from "./runtime-machines";
import { MachineCliSection } from "./machine-cli-section";

const RESOURCES = { en: { common: enCommon, runtimes: enRuntimes } };

function machine(overrides: Partial<RuntimeMachine> = {}): RuntimeMachine {
  return {
    id: "local:daemon-1",
    daemonId: "daemon-1",
    title: "dev.local",
    subtitle: null,
    deviceInfo: "dev.local",
    cliVersion: "0.3.17",
    launchedBy: null,
    mode: "local",
    section: "remote",
    isCurrent: false,
    health: "online",
    runtimes: [],
    onlineCount: 0,
    issueCount: 0,
    runningCount: 0,
    queuedCount: 0,
    providerNames: [],
    lastSeenAt: "2026-07-15T00:00:00Z",
    ...overrides,
  };
}

function renderSection(m: RuntimeMachine) {
  return render(
    <I18nProvider locale="en" resources={RESOURCES}>
      <MachineCliSection machine={m} />
    </I18nProvider>,
  );
}

describe("MachineCliSection", () => {
  it("shows the reported CLI version", () => {
    renderSection(machine());
    expect(screen.getByText("0.3.17")).toBeInTheDocument();
  });

  it("renders nothing when the machine reports no CLI version", () => {
    const { container } = renderSection(machine({ cliVersion: null }));
    expect(container).toBeEmptyDOMElement();
  });
});
