import type { CloudConfig } from "./types";

/** Password policy advertised by the relay; relay versions before this field used eight. */
export function passwordMinLength(config: CloudConfig): number {
  const value = config.password_min_len;
  return typeof value === "number" && Number.isFinite(value) && value > 0 ? Math.floor(value) : 8;
}
