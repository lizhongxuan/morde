// @vitest-environment node
import { describe, expect, it } from "vitest";
import { handlers } from "./handlers";

const baseUrl = "http://localhost";
let requestNumber = 0;

async function resolveHandler(method: string, path: string, request: Request): Promise<Response> {
  const handler = handlers.find((candidate) => {
    const info = candidate.info as { method?: string; path?: string };
    const expected = (info.path ?? "").split("/");
    const actual = path.split("/");
    return info.method === method && expected.length === actual.length && expected.every((part, index) => part.startsWith(":") || part === actual[index]);
  });
  if (!handler) throw new Error(`Missing mock handler: ${method} ${path}`);
  const result = await handler.run({
    request: request as Parameters<typeof handler.run>[0]["request"],
    requestId: `local-${++requestNumber}`,
    resolutionContext: { baseUrl },
  });
  if (!result?.response) throw new Error(`Mock handler did not resolve: ${method} ${path}`);
  return result.response;
}

async function upload(file: File, path: string) {
  const form = new FormData();
  form.append("file", file);
  form.append("path", path);
  const response = await resolveHandler("POST", "/v1/file/upload", new Request(`${baseUrl}/v1/file/upload`, { method: "POST", body: form }));
  expect(response.ok).toBe(true);
  expect((await response.json() as { success: boolean }).success).toBe(true);
}

async function listPath(path: string, showHidden = false) {
  const response = await resolveHandler("POST", "/v1/file/list", new Request(`${baseUrl}/v1/file/list`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ path, show_hidden: showHidden }),
  }));
  return await response.json() as { success: boolean; message: string; data: { files: Array<{ name: string; path?: string; is_directory?: boolean; size?: number | null; extension?: string | null }> } };
}

async function list(showHidden: boolean) {
  return listPath("/home/gem/workspace", showHidden);
}

async function exec(command: string) {
  const response = await resolveHandler("POST", "/v1/shell/exec", new Request(`${baseUrl}/v1/shell/exec`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ command }),
  }));
  return await response.json() as { success: boolean; data: { exit_code: number } };
}

describe("workbench mock file handlers", () => {
  it("returns the safe Codex readiness summary without CLI output", async () => {
    const response = await resolveHandler("GET", "/__ch/api/applications", new Request(`${baseUrl}/__ch/api/applications`));
    expect(response.status).toBe(200);
    expect(await response.json()).toMatchObject({
      host_id: "h1234567",
      items: expect.arrayContaining([
        { id: "codex", installed: true, version: "0.1.0", login_status: "not_logged_in", installable: false, install_operation: null },
        { id: "wechat", installed: false, version: "", login_status: "not_applicable", installable: true, install_operation: null },
      ]),
    });
  });

  it("accepts WeChat install requests idempotently and exposes the operation for polling", async () => {
    const requestId = `mock-install-${Date.now()}`;
    const request = () => new Request(`${baseUrl}/__ch/api/applications/wechat/install`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ request_id: requestId }),
    });
    const first = await resolveHandler("POST", "/__ch/api/applications/wechat/install", request());
    const second = await resolveHandler("POST", "/__ch/api/applications/wechat/install", request());
    const firstBody = await first.json() as { operation: { id: string; status: string } };
    const secondBody = await second.json() as { operation: { id: string; status: string } };
    const status = await resolveHandler("GET", "/__ch/api/applications", new Request(`${baseUrl}/__ch/api/applications`));

    expect(first.status).toBe(202);
    expect(second.status).toBe(202);
    expect(secondBody.operation.id).toBe(firstBody.operation.id);
    expect(firstBody.operation.status).toBe("accepted");
    expect(await status.json()).toMatchObject({ items: expect.arrayContaining([
      expect.objectContaining({ id: "wechat", install_operation: expect.objectContaining({ id: firstBody.operation.id, status: "running" }) }),
    ]) });
  });

  it("serves launches through the generic application routes and rejects unlisted apps", async () => {
    await resolveHandler("GET", "/__ch/api/host", new Request(`${baseUrl}/__ch/api/host`));
    const launchId = `launch_mock-${Date.now()}`;
    const launchRequest = () => new Request(`${baseUrl}/__ch/api/applications/wechat/launch`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ launch_id: launchId }),
    });
    const first = await resolveHandler("POST", "/__ch/api/applications/wechat/launch", launchRequest());
    const replay = await resolveHandler("POST", "/__ch/api/applications/wechat/launch", launchRequest());
    const current = await resolveHandler("GET", "/__ch/api/applications/wechat/launches/current", new Request(`${baseUrl}/__ch/api/applications/wechat/launches/current`));

    expect(first.status).toBe(202);
    expect(replay.status).toBe(202);
    expect(await replay.json()).toMatchObject({ operation: { id: launchId, application_id: "wechat", status: "starting" } });
    expect(await current.json()).toMatchObject({ operation: { id: launchId } });

    const unknownApp = await resolveHandler("POST", "/__ch/api/applications/qq/launch", new Request(`${baseUrl}/__ch/api/applications/qq/launch`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ launch_id: `launch_qq-${Date.now()}` }),
    }));
    expect(unknownApp.status).toBe(404);
  });

  it("uses null size and extension metadata for directory entries", async () => {
    expect((await listPath("/home/gem")).data.files).toContainEqual(expect.objectContaining({
      name: "workspace",
      path: "/home/gem/workspace",
      is_directory: true,
      size: null,
      extension: null,
    }));
  });

  it("hides dotfiles unless show_hidden is true", async () => {
    const hiddenFile = "/home/gem/workspace/.mock-hidden-file";
    const hiddenDirectory = "/home/gem/workspace/.mock-hidden-directory";
    await upload(new File(["secret"], ".mock-hidden-file"), hiddenFile);
    const mkdir = await resolveHandler("POST", "/v1/shell/exec", new Request(`${baseUrl}/v1/shell/exec`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ command: `mkdir -- '${hiddenDirectory}'` }),
    }));
    expect(mkdir.ok).toBe(true);

    const hidden = [".mock-hidden-file", ".mock-hidden-directory"];
    expect((await list(false)).data.files.map((entry) => entry.name)).not.toEqual(expect.arrayContaining(hidden));
    expect((await list(true)).data.files.map((entry) => entry.name)).toEqual(expect.arrayContaining(hidden));
  });

  it("preserves uploaded binary bytes and reads text files as UTF-8", async () => {
    const binaryPath = "/home/gem/workspace/mock-binary-fixture.bin";
    const bytes = Uint8Array.from([0, 255, 195, 40, 128, 0, 65]);
    await upload(new File([bytes], "mock-binary-fixture.bin"), binaryPath);

    const download = await resolveHandler("GET", "/v1/file/download", new Request(`${baseUrl}/v1/file/download?path=${encodeURIComponent(binaryPath)}`));
    expect(download.ok).toBe(true);
    expect(new Uint8Array(await download.arrayBuffer())).toEqual(bytes);

    const textPath = "/home/gem/workspace/mock-text-fixture.txt";
    await upload(new File(["hello\n你好"], "mock-text-fixture.txt"), textPath);
    const read = await resolveHandler("POST", "/v1/file/read", new Request(`${baseUrl}/v1/file/read`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ file: textPath }),
    }));
    const result = await read.json() as { data: { content: string } };
    expect(result.data.content).toBe("hello\n你好");

    const binaryRead = await resolveHandler("POST", "/v1/file/read", new Request(`${baseUrl}/v1/file/read`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ file: binaryPath }),
    }));
    expect(await binaryRead.json()).toMatchObject({
      success: false,
      message: "文件不是有效的 UTF-8 文本，无法预览",
      data: { error_type: "decode_error" },
    });
  });

  it("reports missing directories and rejects writes without a parent directory", async () => {
    const missing = "/home/gem/workspace/mock-missing-parent";
    expect(await listPath(missing)).toMatchObject({ success: false, message: "目录不存在" });

    const filePath = `${missing}/file.txt`;
    const writeResponse = await resolveHandler("POST", "/v1/file/write", new Request(`${baseUrl}/v1/file/write`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ file: filePath, content: "should not be written" }),
    }));
    expect(await writeResponse.json()).toMatchObject({ success: false, message: "父目录不存在" });

    const form = new FormData();
    form.append("file", new File(["should not upload"], "file.txt"));
    form.append("path", filePath);
    const uploadResponse = await resolveHandler("POST", "/v1/file/upload", new Request(`${baseUrl}/v1/file/upload`, { method: "POST", body: form }));
    expect(await uploadResponse.json()).toMatchObject({ success: false, message: "父目录不存在" });
    const download = await resolveHandler("GET", "/v1/file/download", new Request(`${baseUrl}/v1/file/download?path=${encodeURIComponent(filePath)}`));
    expect(download.status).toBe(404);
  });

  it("rejects uploads onto directories while allowing existing files to be replaced", async () => {
    const directoryPath = "/home/gem/workspace/mock-upload-directory-conflict";
    const directoryName = directoryPath.split("/").at(-1)!;
    expect(await exec(`mkdir -- '${directoryPath}'`)).toMatchObject({ success: true, data: { exit_code: 0 } });

    const directoryForm = new FormData();
    directoryForm.append("file", new File(["file contents"], directoryName));
    directoryForm.append("path", directoryPath);
    const directoryUpload = await resolveHandler("POST", "/v1/file/upload", new Request(`${baseUrl}/v1/file/upload`, { method: "POST", body: directoryForm }));
    expect(await directoryUpload.json()).toMatchObject({ success: false, message: "目标路径是目录，无法覆盖" });
    expect((await list(false)).data.files).toContainEqual(expect.objectContaining({ name: directoryName, is_directory: true }));
    expect((await resolveHandler("GET", "/v1/file/download", new Request(`${baseUrl}/v1/file/download?path=${encodeURIComponent(directoryPath)}`))).status).toBe(404);

    const filePath = "/home/gem/workspace/mock-upload-overwrite.txt";
    await upload(new File(["old contents"], "mock-upload-overwrite.txt"), filePath);
    const fileForm = new FormData();
    fileForm.append("file", new File(["new contents"], "mock-upload-overwrite.txt"));
    fileForm.append("path", filePath);
    const fileUpload = await resolveHandler("POST", "/v1/file/upload", new Request(`${baseUrl}/v1/file/upload`, { method: "POST", body: fileForm }));
    expect(await fileUpload.json()).toMatchObject({ success: true });
    const fileDownload = await resolveHandler("GET", "/v1/file/download", new Request(`${baseUrl}/v1/file/download?path=${encodeURIComponent(filePath)}`));
    expect(await fileDownload.text()).toBe("new contents");
  });

  it("returns nonzero for missing rm paths, existing mkdir targets, and missing mkdir parents without creating entries", async () => {
    const missingParent = "/home/gem/workspace/mock-missing-parent-for-mkdir";
    expect(await exec("rm -r -- '/home/gem/workspace/mock-missing-entry'")).toMatchObject({ success: true, data: { exit_code: 1 } });
    expect(await exec("mkdir -- '/home/gem/workspace'")).toMatchObject({ success: true, data: { exit_code: 1 } });
    expect(await exec(`mkdir -- '${missingParent}/child'`)).toMatchObject({ success: true, data: { exit_code: 1 } });
    expect((await list(false)).data.files.map((entry) => entry.name)).not.toContain("mock-missing-parent-for-mkdir");
    expect(await exec("echo 'not implemented'")).toMatchObject({ success: true, data: { exit_code: 127 } });
  });

  it("keeps the UI mkdir, mv, and recursive rm flow working", async () => {
    const directory = "/home/gem/workspace/mock-ui-command-flow";
    const source = `${directory}/source.txt`;
    const target = `${directory}/renamed.txt`;
    expect(await exec(`mkdir -- '${directory}'`)).toMatchObject({ success: true, data: { exit_code: 0 } });
    await upload(new File(["move me"], "source.txt"), source);

    expect(await exec(`mv -n -- '${source}' '${target}'`)).toMatchObject({ success: true, data: { exit_code: 0 } });
    const sourceDownload = await resolveHandler("GET", "/v1/file/download", new Request(`${baseUrl}/v1/file/download?path=${encodeURIComponent(source)}`));
    const targetDownload = await resolveHandler("GET", "/v1/file/download", new Request(`${baseUrl}/v1/file/download?path=${encodeURIComponent(target)}`));
    expect(sourceDownload.status).toBe(404);
    expect(targetDownload.status).toBe(200);

    expect(await exec(`rm -r -- '${directory}'`)).toMatchObject({ success: true, data: { exit_code: 0 } });
    const removedDownload = await resolveHandler("GET", "/v1/file/download", new Request(`${baseUrl}/v1/file/download?path=${encodeURIComponent(target)}`));
    expect(removedDownload.status).toBe(404);
    expect((await list(false)).data.files.map((entry) => entry.name)).not.toContain("mock-ui-command-flow");
  });

  it("treats the first -- as an option delimiter and preserves a later -- operand", async () => {
    expect(await exec("mkdir -- --")).toMatchObject({ success: true, data: { exit_code: 0 } });
    expect(await exec("mv -- -- /home/gem/workspace/mock-dashdash"))
      .toMatchObject({ success: true, data: { exit_code: 0 } });
    expect(await exec("rm -r -- /home/gem/workspace/mock-dashdash"))
      .toMatchObject({ success: true, data: { exit_code: 0 } });
  });
});
