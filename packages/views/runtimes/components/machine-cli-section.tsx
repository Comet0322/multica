import { useT } from "../../i18n";
import type { RuntimeMachine } from "./runtime-machines";

export function MachineCliSection({ machine }: { machine: RuntimeMachine }) {
  const { t } = useT("runtimes");

  if (!machine.cliVersion) return null;

  return (
    <span className="flex items-center gap-2">
      <span>{t(($) => $.update.cli_version_label)}</span>
      <span className="font-mono">{machine.cliVersion}</span>
    </span>
  );
}
