import { createReadStream, promises as fs } from "node:fs";
import { createServer, request as httpRequest, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { request as httpsRequest } from "node:https";
import { once } from "node:events";
import { extname, isAbsolute, join, relative, resolve, sep } from "node:path";
import { pipeline } from "node:stream/promises";

const blockedPaths = ["/admin", "/admin/", "/app", "/app/"];
const hopHeaders = new Set(["connection", "host", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade"]);

export type LocalPortalServer = {
  origin: string;
  serviceOrigin: string;
  setHostFrameSource(value: string | null): void;
  close(): Promise<void>;
};

function frameSource(value: string): string {
  const match = /^https?:\/\/\*\.([a-z0-9_.-]+)(?::(\d{1,5}))?$/i.exec(value);
  const hostname = match?.[1] ?? "";
  const port = match?.[2];
  const labels = hostname.split(".");
  if (!match || !hostname || hostname.length > 253 || labels.some((label) => !label || label.length > 63 || label.startsWith("-") || label.endsWith("-")) ||
    port && (Number(port) < 1 || Number(port) > 65535)) throw new Error("Invalid Morde Host frame source");
  return value;
}

function contentType(path: string): string {
  switch (extname(path).toLowerCase()) {
    case ".css": return "text/css; charset=utf-8";
    case ".html": return "text/html; charset=utf-8";
    case ".js": case ".mjs": return "text/javascript; charset=utf-8";
    case ".json": case ".map": return "application/json; charset=utf-8";
    case ".png": return "image/png";
    case ".svg": return "image/svg+xml";
    case ".wasm": return "application/wasm";
    case ".gz": return "application/gzip";
    case ".woff": return "font/woff";
    case ".woff2": return "font/woff2";
    default: return "application/octet-stream";
  }
}

function writeError(response: ServerResponse, status: number, message: string): void {
  const body = JSON.stringify({ error: message });
  response.writeHead(status, { "content-type": "application/json; charset=utf-8", "content-length": Buffer.byteLength(body), "cache-control": "no-store", "x-content-type-options": "nosniff" });
  response.end(body);
}

function cookieForPortal(value: string | undefined): string | undefined {
  const cookie = value?.split(";").map((item) => item.trim()).find((item) => item.startsWith("ch_portal="));
  return cookie;
}

function proxyRequest(request: IncomingMessage, response: ServerResponse, service: URL, origin: string, localURL: URL): void {
  const headers: Record<string, string | string[] | undefined> = {};
  for (const [name, value] of Object.entries(request.headers)) {
    if (hopHeaders.has(name.toLowerCase()) || name.toLowerCase() === "cookie" || name.toLowerCase() === "origin" || name.toLowerCase() === "referer") continue;
    headers[name] = value;
  }
  const cookie = cookieForPortal(request.headers.cookie);
  if (cookie) headers.cookie = cookie;
  headers.origin = service.origin;
  if (request.headers.referer) {
    try {
      const referer = new URL(request.headers.referer);
      if (referer.origin === origin) headers.referer = `${service.origin}${referer.pathname}${referer.search}`;
    } catch { /* Drop malformed referrers at the service boundary. */ }
  }

  const upstreamURL = new URL(`${localURL.pathname}${localURL.search}`, service);
  const transport = upstreamURL.protocol === "https:" ? httpsRequest : httpRequest;
  const upstream = transport(upstreamURL, { method: request.method, headers }, (upstreamResponse) => {
    const responseHeaders: Record<string, string | string[] | undefined> = {};
    for (const [name, value] of Object.entries(upstreamResponse.headers)) {
      if (!hopHeaders.has(name.toLowerCase())) responseHeaders[name] = value;
    }
    response.writeHead(upstreamResponse.statusCode ?? 502, responseHeaders);
    upstreamResponse.pipe(response);
  });
  upstream.on("error", () => {
    if (!response.headersSent) writeError(response, 502, "Morde service is unavailable");
    else response.destroy();
  });
  request.pipe(upstream);
}

export async function startLocalPortalServer(directory: string, serviceOrigin: string, hostFrameSource?: string): Promise<LocalPortalServer> {
  if (!isAbsolute(directory)) throw new Error("Portal asset directory must be absolute");
  const service = new URL(serviceOrigin);
  if ((service.protocol !== "https:" && !(service.protocol === "http:" && ["localhost", "portal.localhost", "127.0.0.1", "[::1]"].includes(service.hostname))) ||
    service.username || service.password || service.pathname !== "/" || service.search || service.hash) throw new Error("Invalid Morde service origin");
  let allowedFrameSource = hostFrameSource ? frameSource(hostFrameSource) : "";
  const root = resolve(directory);
  if (!(await fs.stat(root)).isDirectory()) throw new Error("Portal assets are unavailable");
  let origin = "";
  const server: Server = createServer((request, response) => {
    const host = request.headers.host;
    if (!origin || host !== origin.slice("http://".length)) { writeError(response, 403, "Forbidden"); return; }
    let url: URL;
    try { url = new URL(request.url ?? "/", origin); } catch { writeError(response, 400, "Invalid request"); return; }
    if (url.origin !== origin || url.username || url.password) { writeError(response, 403, "Forbidden"); return; }
    const requestOrigin = request.headers.origin;
    if (requestOrigin && requestOrigin !== origin) { writeError(response, 403, "Forbidden"); return; }
    if (request.headers["sec-fetch-site"] === "cross-site") { writeError(response, 403, "Forbidden"); return; }
    if (blockedPaths.some((path) => url.pathname === path || url.pathname.startsWith(`${path}/`))) { writeError(response, 404, "Not found"); return; }

    if (url.pathname === "/api" || url.pathname.startsWith("/api/") || url.pathname === "/mcp" || url.pathname.startsWith("/mcp/") ||
      url.pathname === "/downloads" || url.pathname.startsWith("/downloads/")) {
      if (request.method !== "GET" && request.method !== "HEAD" && requestOrigin !== origin) { writeError(response, 403, "Origin required"); return; }
      proxyRequest(request, response, service, origin, url);
      return;
    }

    if (request.method !== "GET" && request.method !== "HEAD") { writeError(response, 405, "Method not allowed"); return; }
    let pathname: string;
    try { pathname = decodeURIComponent(url.pathname); } catch { writeError(response, 400, "Invalid path"); return; }
    const route = pathname === "/" || !extname(pathname);
    if (!route && !pathname.startsWith("/assets/") && !pathname.startsWith("/ocr/")) { writeError(response, 404, "Not found"); return; }
    const relativePath = route ? "index.html" : pathname.slice(1);
    const filePath = resolve(root, relativePath);
    const fileRelative = relative(root, filePath);
    if (!fileRelative || fileRelative.startsWith(`..${sep}`) || isAbsolute(fileRelative)) { writeError(response, 404, "Not found"); return; }
    void fs.stat(filePath).then(async (info) => {
      if (!info.isFile()) { writeError(response, 404, "Not found"); return; }
      response.setHeader("content-type", contentType(filePath));
      response.setHeader("content-length", info.size);
      response.setHeader("cache-control", "no-cache");
      response.setHeader("x-content-type-options", "nosniff");
      if (relativePath === "index.html") {
        response.setHeader("content-security-policy", `default-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'self'; object-src 'none'; frame-src 'self'${allowedFrameSource ? ` ${allowedFrameSource}` : ""}`);
        let html = await fs.readFile(filePath, "utf8");
        const escapedOrigin = service.origin.replaceAll("&", "&amp;").replaceAll("\"", "&quot;");
        html = html.replace(/<head>/i, `<head><meta name="morde-service-origin" content="${escapedOrigin}">`);
        response.setHeader("content-length", Buffer.byteLength(html));
        if (request.method === "HEAD") { response.end(); return; }
        response.end(html);
        return;
      }
      if (request.method === "HEAD") { response.end(); return; }
      await pipeline(createReadStream(filePath), response);
    }).catch(() => {
      if (!response.headersSent) writeError(response, 404, "Not found");
      else response.destroy();
    });
  });

  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  server.unref();
  const address = server.address();
  if (!address || typeof address === "string") { server.close(); throw new Error("Local Portal server failed to start"); }
  origin = `http://127.0.0.1:${address.port}`;
  return {
    origin,
    serviceOrigin: service.origin,
    setHostFrameSource(value) { allowedFrameSource = value ? frameSource(value) : ""; },
    close: async () => {
      if (!server.listening) return;
      server.close();
      server.closeAllConnections();
      await once(server, "close");
    },
  };
}
