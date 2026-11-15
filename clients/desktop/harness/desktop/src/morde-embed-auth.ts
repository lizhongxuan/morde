import { localGatewayOrigin, type HostEmbedTarget } from "./morde-core";

export type ExchangedHostEmbedSession = {
  url: string;
  cookie: { value: string; secure: boolean; expirationDate: number };
};

export async function exchangeHostEmbedTicket(exchangeURLInput: string, expectedHostOrigin: string, portalOriginInput: string,
  ticket: string, nonce: string, initialTarget: HostEmbedTarget, targets: HostEmbedTarget[]): Promise<ExchangedHostEmbedSession> {
  const exchangeURL = new URL(exchangeURLInput);
  const expectedHost = new URL(expectedHostOrigin);
  if (exchangeURL.origin !== expectedHost.origin || exchangeURL.pathname !== "/__ch/embed-auth" || exchangeURL.search || exchangeURL.hash ||
    exchangeURL.username || exchangeURL.password || (exchangeURL.protocol !== "https:" && !localGatewayOrigin(exchangeURL, portalOriginInput)) ||
    !/^[\w-]{43}$/.test(ticket) || !/^[\w-]{32}$/.test(nonce) || !targets.includes(initialTarget)) {
    throw new Error("Invalid Host embed exchange request");
  }
  let response: Response;
  try {
    response = await fetch(exchangeURL, {
      method: "POST", redirect: "manual", signal: AbortSignal.timeout(15_000),
      headers: { origin: portalOriginInput, accept: "text/html", "content-type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({ ticket, embed_nonce: nonce }),
    });
  } catch {
    throw new Error("无法连接指定云端电脑，请重试。");
  }
  if (response.status !== 303) throw new Error(`Host embed exchange failed (HTTP ${response.status})`);
  const location = response.headers.get("location");
  if (!location) throw new Error("Host embed exchange omitted its Workbench location");
  const targetURL = new URL(location, exchangeURL);
  const query = targetURL.searchParams;
  if (targetURL.origin !== expectedHost.origin || targetURL.username || targetURL.password ||
    targetURL.pathname !== "/__ch/wb/" || targetURL.hash ||
    query.getAll("embed").length !== 1 || query.get("embed") !== "1" ||
    query.getAll("embed_nonce").length !== 1 || query.get("embed_nonce") !== nonce ||
    query.getAll("tab").length !== 1 || query.get("tab") !== initialTarget ||
    [...query.keys()].some((key) => !["embed", "embed_nonce", "tab"].includes(key))) {
    throw new Error("Host embed exchange returned an invalid Workbench location");
  }
  const setCookies = response.headers.getSetCookie();
  if (setCookies.length !== 1 || !setCookies[0]) throw new Error("Host embed exchange returned an invalid session cookie");
  const parts = setCookies[0].split(";").map((part) => part.trim());
  const pair = /^ch_embed=([^;\s]+)$/.exec(parts.shift() ?? "");
  const attributes = new Map<string, string | true>();
  for (const part of parts) {
    const [rawName = "", ...rest] = part.split("=");
    if (!rawName) throw new Error("Host embed exchange returned an invalid cookie attribute");
    const name = rawName.toLowerCase();
    if (attributes.has(name)) throw new Error("Host embed exchange returned duplicate cookie attributes");
    attributes.set(name, rest.length ? rest.join("=") : true);
  }
  const maxAge = attributes.get("max-age");
  const expires = attributes.get("expires");
  const maxAgeSeconds = typeof maxAge === "string" && /^\d+$/.test(maxAge) ? Number(maxAge) : Number.NaN;
  const expirationDate = maxAge === undefined
    ? typeof expires === "string" ? Date.parse(expires) / 1000 : Number.NaN
    : Number.isSafeInteger(maxAgeSeconds) && maxAgeSeconds > 0 ? Date.now() / 1000 + maxAgeSeconds : Number.NaN;
  const secure = attributes.has("secure");
  if (!pair || !pair[1] || pair[1].length > 4096 || !attributes.has("httponly") || attributes.get("path") !== "/" ||
    attributes.has("domain") || String(attributes.get("samesite") ?? "").toLowerCase() !== "strict" ||
    expectedHost.protocol === "https:" && !secure || !Number.isFinite(expirationDate) || expirationDate <= Date.now() / 1000) {
    throw new Error("Host embed exchange returned an unsafe session cookie");
  }
  return { url: targetURL.href, cookie: { value: pair[1], secure, expirationDate } };
}
