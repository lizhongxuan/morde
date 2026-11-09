import { cx } from "../util";

/**
 * The app's own mark (docs/brand.md): the N stroke on its white tile with a hairline edge,
 * `public/icon.svg`. It stands for the app where the app speaks for itself — the sign-in
 * door, the first seconds before the socket is up, the token gate — never for the Muse: the
 * face is the Muse, the mark is the app. The tile stays white in the dark theme too.
 */
export function BrandMark({ size = 72, className }: { size?: number; className?: string }) {
  return <img src="/icon.svg" alt="nanoMuse" width={size} height={size} draggable={false} className={cx("block shrink-0 select-none", className)} style={{ width: size, height: size }} />;
}
