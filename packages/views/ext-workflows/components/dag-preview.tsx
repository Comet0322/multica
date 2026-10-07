"use client";

import { useId } from "react";
import { useT } from "../../i18n";
import { layoutDag, type NodeRow } from "../node-editor-helpers";

const MAX_LABEL = 22;

function clip(label: string): string {
  return label.length > MAX_LABEL ? `${label.slice(0, MAX_LABEL - 1)}…` : label;
}

/** Read-only graph: nodes in columns by depth, plain SVG edges. No dependency. */
export function DagPreview({
  rows,
}: {
  rows: readonly Pick<NodeRow, "rowId" | "key" | "title" | "depends_on">[];
}) {
  const { t } = useT("ext-workflows");
  const markerId = `dag-arrow-${useId().replace(/:/g, "")}`;
  const layout = layoutDag(rows);

  if (layout.nodes.length === 0) {
    return <p className="text-caption text-muted-foreground">{t(($) => $.dag.empty)}</p>;
  }

  return (
    <div className="overflow-x-auto rounded-lg border bg-muted/20">
      <svg
        role="img"
        aria-label={t(($) => $.dag.aria_label)}
        width={layout.width}
        height={layout.height}
        viewBox={`0 0 ${layout.width} ${layout.height}`}
      >
        <defs>
          <marker
            id={markerId}
            viewBox="0 0 8 8"
            refX="7"
            refY="4"
            markerWidth="7"
            markerHeight="7"
            orient="auto-start-reverse"
          >
            <path d="M 0 0 L 8 4 L 0 8 z" className="fill-muted-foreground" />
          </marker>
        </defs>
        {layout.edges.map((edge) => (
          <path
            key={`${edge.from}->${edge.to}`}
            data-dag-edge=""
            d={edge.d}
            fill="none"
            strokeWidth={1.5}
            className="stroke-muted-foreground/60"
            markerEnd={`url(#${markerId})`}
          />
        ))}
        {layout.nodes.map((n) => (
          <g key={n.rowId} data-dag-node={n.rowId}>
            {n.label.length > MAX_LABEL && <title>{n.label}</title>}
            <rect x={n.x} y={n.y} width={n.w} height={n.h} rx={8} className="fill-background stroke-border" />
            <text
              x={n.x + n.w / 2}
              y={n.y + n.h / 2}
              textAnchor="middle"
              dominantBaseline="central"
              fontSize={12}
              className="fill-foreground"
            >
              {clip(n.label)}
            </text>
          </g>
        ))}
      </svg>
    </div>
  );
}
