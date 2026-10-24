"use client";

import type { ReactNode } from "react";
import {
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from "@multica/ui/components/ui/tabs";

/**
 * The Multica CLI install command. The CLI is built from a checkout of this
 * repository; there is no hosted installer. Windows is not a supported build
 * target, so the switch lists only the Unix-like platform.
 */
export const CLI_INSTALL_COMMANDS = {
  macosLinux: "cd server && go build -o /usr/local/bin/multica ./cmd/multica",
} as const;

export type CliInstallPlatform = keyof typeof CLI_INSTALL_COMMANDS;

const PLATFORMS = ["macosLinux"] as const satisfies readonly CliInstallPlatform[];

export interface CliInstallCommandLabels {
  /** Accessible name for the platform switch. */
  group: string;
  macosLinux: string;
  /** Accepted for existing callers; Windows is not offered. */
  windows?: string;
}

/**
 * Platform switch for the install command, built on the shared Tabs primitive
 * so tab semantics and arrow-key navigation come for free.
 *
 * The switch is the only shared UI: `children` receives the selected command
 * and renders it, so each surface keeps its own command row — the product
 * surfaces their token-styled row with an icon copy button, the landing page
 * its marketing palette with a spelled-out one.
 */
export function CliInstallCommand({
  labels,
  classNames,
  children,
}: {
  labels: CliInstallCommandLabels;
  /** Palette overrides for surfaces that do not use the product tokens. */
  classNames?: { list?: string; trigger?: string };
  children: (command: string) => ReactNode;
}) {
  return (
    <Tabs defaultValue={PLATFORMS[0]}>
      <TabsList
        aria-label={labels.group}
        activateOnFocus
        className={classNames?.list}
      >
        {PLATFORMS.map((platform) => (
          <TabsTrigger
            key={platform}
            value={platform}
            className={classNames?.trigger}
          >
            {labels[platform]}
          </TabsTrigger>
        ))}
      </TabsList>
      {PLATFORMS.map((platform) => (
        <TabsContent key={platform} value={platform}>
          {children(CLI_INSTALL_COMMANDS[platform])}
        </TabsContent>
      ))}
    </Tabs>
  );
}
