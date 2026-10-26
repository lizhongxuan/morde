import { afterEach, describe, expect, it, vi } from "vitest";
import { AioError } from "./aio";
import { listFiles, MAX_UPLOAD_BYTES, readFile, uploadFile } from "./fileApi";

afterEach(() => vi.unstubAllGlobals());

describe("uploadFile", () => {
  it("rejects files over 100 MB before creating a request", async () => {
    const file = new File(["x"], "large.bin");
    Object.defineProperty(file, "size", { value: MAX_UPLOAD_BYTES + 1 });
    const createRequest = vi.fn();
    vi.stubGlobal("XMLHttpRequest", createRequest);

    await expect(uploadFile(file, "/workspace/large.bin", vi.fn())).rejects.toThrow(
      "单个文件不能超过 100 MB",
    );
    expect(createRequest).not.toHaveBeenCalled();
  });

  it("reports upload progress from the transport event without sending network traffic", async () => {
    class MockXMLHttpRequest {
      static latest: MockXMLHttpRequest;
      constructor() { MockXMLHttpRequest.latest = this; }
      upload = { onprogress: null as ((event: ProgressEvent) => void) | null };
      open = vi.fn();
      send = vi.fn();
      withCredentials = false;
      status = 200;
      responseText = JSON.stringify({ success: true });
      onerror: ((event: ProgressEvent) => void) | null = null;
      onabort: ((event: ProgressEvent) => void) | null = null;
      onload: ((event: ProgressEvent) => void) | null = null;
    }
    vi.stubGlobal("XMLHttpRequest", MockXMLHttpRequest);
    const onProgress = vi.fn();
    const result = uploadFile(new File(["x"], "small.txt"), "/workspace/small.txt", onProgress);
    const request = MockXMLHttpRequest.latest;

    expect(request.open).toHaveBeenCalledWith("POST", "/v1/file/upload");
    expect(request.send).toHaveBeenCalledOnce();
    request.upload.onprogress?.({ lengthComputable: true, loaded: 3, total: 4 } as ProgressEvent);
    expect(onProgress).toHaveBeenCalledWith(75);
    request.onload?.({} as ProgressEvent);
    await expect(result).resolves.toBeUndefined();
  });

  it.each([
    ['string success flag', { success: 'false', message: 'invalid success field' }, 'invalid success field'],
    ['missing success flag', { message: 'missing success field' }, 'missing success field'],
    ['null envelope', null, '上传失败（HTTP 200）'],
  ])('rejects an upload envelope with a %s', async (_name, envelope, message) => {
    class MockXMLHttpRequest {
      static latest: MockXMLHttpRequest;
      constructor() { MockXMLHttpRequest.latest = this; }
      upload = { onprogress: null as ((event: ProgressEvent) => void) | null };
      open = vi.fn();
      send = vi.fn();
      withCredentials = false;
      status = 200;
      responseText = JSON.stringify(envelope);
      onerror: ((event: ProgressEvent) => void) | null = null;
      onabort: ((event: ProgressEvent) => void) | null = null;
      onload: ((event: ProgressEvent) => void) | null = null;
    }
    vi.stubGlobal("XMLHttpRequest", MockXMLHttpRequest);

    const result = uploadFile(new File(["x"], "small.txt"), "/workspace/small.txt", vi.fn());
    MockXMLHttpRequest.latest.onload?.({} as ProgressEvent);

    await expect(result).rejects.toThrow(message);
  });
});

describe("readFile", () => {
  it.each([
    ["missing content", { file: "/workspace/file.txt" }],
    ["non-string content", { content: null }],
  ])("rejects a successful envelope with %s", async (_name, data) => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({
      success: true,
      message: "ok",
      data,
    }))));

    await expect(readFile("/workspace/file.txt")).rejects.toThrow("文件读取响应无效");
  });
});

describe("listFiles", () => {
  it.each([
    ["missing data", { success: true, message: "ok" }],
    ["null data", { success: true, message: "ok", data: null }],
    ["non-object data", { success: true, message: "ok", data: [] }],
    ["missing files", { success: true, message: "ok", data: {} }],
    ["null files", { success: true, message: "ok", data: { files: null } }],
    ["non-array files", { success: true, message: "ok", data: { files: {} } }],
  ])("rejects an invalid directory response with %s", async (_name, envelope) => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify(envelope))));

    const error = await listFiles("/workspace", false).catch((reason: unknown) => reason);
    expect(error).toBeInstanceOf(AioError);
    expect(error).toHaveProperty("message", "目录列表响应无效");
  });

  it.each([
    ["non-object entry", null],
    ["missing name", { path: "/workspace/file.txt", is_directory: false }],
    ["missing path", { name: "file.txt", is_directory: false }],
    ["invalid directory flag", { name: "file.txt", path: "/workspace/file.txt", is_directory: "false" }],
    ["invalid size", { name: "file.txt", path: "/workspace/file.txt", is_directory: false, size: "1" }],
    ["invalid extension", { name: "file.txt", path: "/workspace/file.txt", is_directory: false, extension: 1 }],
  ])("rejects a malformed directory entry with %s", async (_name, entry) => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({
      success: true,
      message: "ok",
      data: { files: [entry] },
    }))));

    await expect(listFiles("/workspace", false)).rejects.toThrow("目录列表响应无效");
  });

  it("accepts null size and extension metadata for AIO directories", async () => {
    const files = [{
      name: "Downloads",
      path: "/home/gem/workspace/Downloads",
      is_directory: true,
      size: null,
      modified_time: "2026-10-20T00:00:00Z",
      extension: null,
    }];
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({
      success: true,
      message: "ok",
      data: { path: "/home/gem/workspace", files },
    }))));

    await expect(listFiles("/home/gem/workspace", false)).resolves.toEqual(files);
  });

  it("returns the files array, including an empty array", async () => {
    const files = [{ name: "README.md", path: "/workspace/README.md", is_directory: false }];
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({
      success: true,
      message: "ok",
      data: { files },
    })));
    vi.stubGlobal("fetch", fetchMock);

    await expect(listFiles("/workspace", false)).resolves.toEqual(files);
    expect(fetchMock).toHaveBeenCalledWith("/v1/file/list", expect.objectContaining({
      body: JSON.stringify({ path: "/workspace", recursive: false, show_hidden: false, include_size: true }),
    }));
  });
});
