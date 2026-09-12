import { Label } from "@primer/react";
import type { LabelProps } from "@primer/react";
import {
  AlertFillIcon,
  CheckCircleFillIcon,
  XCircleFillIcon,
} from "@primer/octicons-react";

export type DevStatusState = "ok" | "down" | "unknown";

const variantByState: Record<DevStatusState, LabelProps["variant"]> = {
  ok: "success",
  down: "danger",
  unknown: "attention",
};

const iconByState = {
  ok: CheckCircleFillIcon,
  down: XCircleFillIcon,
  unknown: AlertFillIcon,
} as const;

export interface DevStatusProps {
  /** Display name of the surface the label describes, e.g. "Go API". */
  name: string;
  state: DevStatusState;
}

/**
 * GitHub-style status label for POST dev/status surfaces (Primer + Octicons,
 * ADR-012). Small shared component proving the workspace package contract.
 */
export function DevStatus({ name, state }: DevStatusProps) {
  const Icon = iconByState[state];
  return (
    <Label variant={variantByState[state]}>
      <span style={{ display: "inline-flex", alignItems: "center", gap: "4px" }}>
        <Icon size={12} aria-hidden />
        {name}
      </span>
    </Label>
  );
}
