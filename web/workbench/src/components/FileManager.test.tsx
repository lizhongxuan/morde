import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { runShell } from "../app/aio";
import { listFiles, MAX_UPLOAD_BYTES, readFile, uploadFile, writeFile } from "../app/fileApi";
import FileManager from "./FileManager";

vi.mock("../app/fileApi", () => ({
  downloadFile: vi.fn(),
  listFiles: vi.fn(),
  MAX_UPLOAD_BYTES: 100 * 1024 * 1024,
  readFile: vi.fn(),
  uploadFile: vi.fn(),
  writeFile: vi.fn(),
}));
vi.mock("../app/aio", () => ({ runShell: vi.fn() }));

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});
beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(listFiles).mockResolvedValue([]);
});

describe("FileManager upload", () => {
  it("shows the progress event percentage while the upload is pending", async () => {
    let reportProgress!: (percent: number) => void;
    let finishUpload!: () => void;
    vi.mocked(uploadFile).mockImplementation((_file, _path, onProgress) => {
      reportProgress = onProgress;
      return new Promise<void>((resolve) => { finishUpload = resolve; });
    });
    render(<FileManager />);

    await waitFor(() => expect(listFiles).toHaveBeenCalled());
    fireEvent.change(screen.getByLabelText(/单文件最大 100 MB/), {
      target: { files: [new File(["x"], "small.txt", { type: "text/plain" })] },
    });
    const progress = await screen.findByLabelText("文件上传进度");
    expect((progress as HTMLProgressElement).value).toBe(0);
    await act(async () => reportProgress(64));
    expect((progress as HTMLProgressElement).value).toBe(64);
    expect(screen.getByText("64%")).toBeInTheDocument();
    await act(async () => finishUpload());
    expect(await screen.findByText("small.txt 上传完成")).toBeInTheDocument();
  });

  it("locks the file picker and drop zone while an upload preflight is pending", async () => {
    let finishPreflight!: (entries: Awaited<ReturnType<typeof listFiles>>) => void;
    let finishUpload!: () => void;
    const preflight = new Promise<Awaited<ReturnType<typeof listFiles>>>((resolve) => {
      finishPreflight = resolve;
    });
    vi.mocked(listFiles)
      .mockResolvedValueOnce([])
      .mockReturnValueOnce(preflight)
      .mockResolvedValueOnce([]);
    vi.mocked(uploadFile).mockImplementation(() => new Promise<void>((resolve) => { finishUpload = resolve; }));
    render(<FileManager />);

    await waitFor(() => expect(listFiles).toHaveBeenCalledTimes(1));
    const picker = screen.getByLabelText(/单文件最大 100 MB/);
    fireEvent.change(picker, { target: { files: [new File(["first"], "same.txt")] } });
    await waitFor(() => expect(listFiles).toHaveBeenCalledTimes(2));
    expect(picker).toBeDisabled();

    const dropZone = document.querySelector(".upload-dropzone");
    fireEvent.drop(dropZone!, {
      dataTransfer: { files: [new File(["second"], "same.txt")] },
    });
    expect(listFiles).toHaveBeenCalledTimes(2);

    await act(async () => finishPreflight([]));
    await waitFor(() => expect(uploadFile).toHaveBeenCalledTimes(1));
    await act(async () => finishUpload());
    expect(await screen.findByText("same.txt 上传完成")).toBeInTheDocument();
    await waitFor(() => expect(listFiles).toHaveBeenCalledTimes(3));
  });

  it("disables conflicting file operations while uploading over the selected file", async () => {
    let finishUpload!: () => void;
    const path = "/home/gem/workspace/README.md";
    const entries = [
      { name: "README.md", path, is_directory: false },
      { name: "folder", path: "/home/gem/workspace/folder", is_directory: true },
    ];
    vi.spyOn(window, "confirm").mockReturnValue(true);
    vi.mocked(listFiles)
      .mockResolvedValueOnce(entries)
      .mockResolvedValueOnce(entries)
      .mockResolvedValueOnce(entries);
    vi.mocked(readFile).mockResolvedValueOnce("existing contents").mockResolvedValueOnce("replacement contents");
    vi.mocked(uploadFile).mockImplementation(() => new Promise<void>((resolve) => { finishUpload = resolve; }));
    render(<FileManager />);

    fireEvent.click(await screen.findByRole("button", { name: "README.md" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "保存" })).toBeEnabled());
    fireEvent.change(screen.getByLabelText(/单文件最大 100 MB/), {
      target: { files: [new File(["replacement"], "README.md")] },
    });
    await waitFor(() => expect(uploadFile).toHaveBeenCalledOnce());

    expect(window.confirm).toHaveBeenCalledWith("“README.md”已存在，确定覆盖吗？");
    expect(screen.getByRole("button", { name: "保存" })).toBeDisabled();
    expect(screen.getByLabelText("重命名所选项")).toBeDisabled();
    expect(screen.getByRole("button", { name: "重命名" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "删除" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "下载" })).toBeDisabled();
    expect(screen.getByLabelText("新建目录")).toBeDisabled();
    expect(screen.getByRole("button", { name: "创建" })).toBeDisabled();
    expect(screen.getByLabelText("当前路径")).toBeDisabled();
    expect(screen.getByLabelText("显示隐藏文件")).toBeDisabled();
    expect(screen.getByRole("button", { name: "上级目录" })).toBeDisabled();
    const openButtons = screen.getAllByRole("button", { name: "打开" });
    expect(openButtons).toHaveLength(2);
    expect(openButtons.every((button) => (button as HTMLButtonElement).disabled)).toBe(true);

    fireEvent.submit(screen.getByLabelText("重命名所选项").closest("form")!);
    fireEvent.submit(screen.getByLabelText("新建目录").closest("form")!);
    expect(writeFile).not.toHaveBeenCalled();
    expect(runShell).not.toHaveBeenCalled();

    await act(async () => finishUpload());
    expect(await screen.findByText("README.md 上传完成")).toBeInTheDocument();
    await waitFor(() => expect(screen.getByLabelText("当前路径")).toBeEnabled());
    expect(screen.queryByLabelText("文件内容")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "README.md" }));
    expect(await screen.findByLabelText("文件内容")).toHaveValue("replacement contents");
  });

  it("shows the size limit and does not call the uploader for an oversized file", async () => {
    const file = new File(["x"], "large.bin");
    Object.defineProperty(file, "size", { value: MAX_UPLOAD_BYTES + 1 });
    render(<FileManager />);

    fireEvent.change(screen.getByLabelText(/单文件最大 100 MB/), { target: { files: [file] } });
    expect(await screen.findByRole("alert")).toHaveTextContent("large.bin 超过 100 MB，未上传");
    expect(uploadFile).not.toHaveBeenCalled();
  });

  it("checks hidden entries before allowing an overwrite", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(false);
    vi.mocked(listFiles)
      .mockResolvedValueOnce([])
      .mockResolvedValueOnce([{
        name: ".env",
        path: "/home/gem/workspace/.env",
        is_directory: false,
      }])
      .mockResolvedValueOnce([]);
    render(<FileManager />);

    await waitFor(() => expect(listFiles).toHaveBeenCalledTimes(1));
    fireEvent.change(screen.getByLabelText(/单文件最大 100 MB/), {
      target: { files: [new File(["new"], ".env")] },
    });

    await waitFor(() => expect(listFiles).toHaveBeenCalledTimes(3));
    expect(listFiles).toHaveBeenNthCalledWith(2, "/home/gem/workspace", true);
    expect(window.confirm).toHaveBeenCalledWith("“.env”已存在，确定覆盖吗？");
    expect(uploadFile).not.toHaveBeenCalled();
  });

  it("rejects an upload whose name matches a directory without asking to overwrite", async () => {
    const directory = {
      name: "assets",
      path: "/home/gem/workspace/assets",
      is_directory: true,
    };
    vi.spyOn(window, "confirm").mockReturnValue(false);
    vi.mocked(listFiles)
      .mockResolvedValueOnce([])
      .mockResolvedValueOnce([directory])
      .mockResolvedValueOnce([directory]);
    render(<FileManager />);

    await waitFor(() => expect(listFiles).toHaveBeenCalledTimes(1));
    fireEvent.change(screen.getByLabelText(/单文件最大 100 MB/), {
      target: { files: [new File(["file contents"], "assets")] },
    });

    expect(await screen.findByRole("alert")).toHaveTextContent("assets 与现有目录同名，未上传");
    expect(window.confirm).not.toHaveBeenCalled();
    expect(uploadFile).not.toHaveBeenCalled();
  });

  it("stops instead of uploading when the preflight directory check fails", async () => {
    vi.mocked(listFiles)
      .mockResolvedValueOnce([])
      .mockRejectedValueOnce(new Error("目录读取失败"))
      .mockResolvedValueOnce([]);
    render(<FileManager />);

    await waitFor(() => expect(listFiles).toHaveBeenCalledTimes(1));
    fireEvent.change(screen.getByLabelText(/单文件最大 100 MB/), {
      target: { files: [new File(["new"], "README.md")] },
    });

    expect(await screen.findByRole("alert")).toHaveTextContent("无法确认目标文件是否已存在，未上传（目录读取失败）");
    expect(uploadFile).not.toHaveBeenCalled();
  });

  it("asks before a repeated filename in the same upload queue", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(false);
    vi.mocked(listFiles)
      .mockResolvedValueOnce([])
      .mockResolvedValueOnce([])
      .mockResolvedValueOnce([]);
    vi.mocked(uploadFile).mockResolvedValue(undefined);
    render(<FileManager />);

    await waitFor(() => expect(listFiles).toHaveBeenCalledTimes(1));
    fireEvent.change(screen.getByLabelText(/单文件最大 100 MB/), {
      target: { files: [new File(["one"], "same.txt"), new File(["two"], "same.txt")] },
    });

    await waitFor(() => expect(listFiles).toHaveBeenCalledTimes(3));
    expect(uploadFile).toHaveBeenCalledTimes(1);
    expect(window.confirm).toHaveBeenCalledExactlyOnceWith("“same.txt”已存在，确定覆盖吗？");
  });
});

describe("FileManager directory requests", () => {
  it("renders AIO directories and unknown file sizes from null metadata", async () => {
    vi.mocked(listFiles).mockResolvedValue([
      {
        name: "Downloads",
        path: "/home/gem/workspace/Downloads",
        is_directory: true,
        size: null,
        extension: null,
      },
      {
        name: "README.md",
        path: "/home/gem/workspace/README.md",
        is_directory: false,
        size: null,
        extension: null,
      },
    ]);
    render(<FileManager />);

    const directoryRow = await screen.findByRole("row", { name: /Downloads/ });
    expect(directoryRow).toHaveTextContent("目录");
    expect(directoryRow).toHaveTextContent("—");
    const fileRow = screen.getByRole("row", { name: /README\.md/ });
    expect(fileRow).toHaveTextContent("文件");
    expect(fileRow).toHaveTextContent("—");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("shows a directory error instead of an empty state and retries the request", async () => {
    vi.mocked(listFiles)
      .mockRejectedValueOnce(new Error("目录服务暂不可用"))
      .mockResolvedValueOnce([]);
    render(<FileManager />);

    expect(await screen.findByRole("alert")).toHaveTextContent("目录服务暂不可用");
    expect(screen.queryByText("此目录为空")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "重试" }));

    expect(await screen.findByText("此目录为空")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(listFiles).toHaveBeenCalledTimes(2);
  });

  it("keeps the editor disabled until the selected file finishes loading", async () => {
    let finishRead!: (contents: string) => void;
    vi.mocked(listFiles).mockResolvedValue([{
      name: "README.md",
      path: "/home/gem/workspace/README.md",
      is_directory: false,
    }]);
    vi.mocked(readFile).mockReturnValue(new Promise((resolve) => { finishRead = resolve; }));
    render(<FileManager />);

    await screen.findByRole("button", { name: "README.md" });
    fireEvent.click(screen.getByRole("button", { name: "README.md" }));
    const editor = screen.getByLabelText("文件内容");
    expect(editor).toBeDisabled();
    expect(screen.getByRole("button", { name: "保存" })).toBeDisabled();

    await act(async () => finishRead("# 当前内容"));
    expect(editor).toHaveValue("# 当前内容");
    expect(editor).toBeEnabled();
    expect(screen.getByRole("button", { name: "保存" })).toBeEnabled();
  });

  it("keeps a file read-only when the text API rejects invalid UTF-8", async () => {
    const path = "/home/gem/workspace/image.bin";
    vi.mocked(listFiles).mockResolvedValue([{
      name: "image.bin",
      path,
      is_directory: false,
    }]);
    vi.mocked(readFile).mockRejectedValue(new Error("文件不是有效的 UTF-8 文本，无法预览"));
    render(<FileManager />);

    fireEvent.click(await screen.findByRole("button", { name: "image.bin" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("文件不是有效的 UTF-8 文本，无法预览");
    expect(screen.getByLabelText("文件内容")).toBeDisabled();
    expect(screen.getByRole("button", { name: "保存" })).toBeDisabled();
    expect(writeFile).not.toHaveBeenCalled();
  });

  it("prevents edits while the selected file is being saved", async () => {
    let finishWrite!: () => void;
    const path = "/home/gem/workspace/README.md";
    vi.mocked(listFiles).mockResolvedValue([{
      name: "README.md",
      path,
      is_directory: false,
    }, {
      name: "folder",
      path: "/home/gem/workspace/folder",
      is_directory: true,
    }]);
    vi.mocked(readFile).mockResolvedValue("initial contents");
    vi.mocked(writeFile).mockImplementation(() => new Promise<void>((resolve) => { finishWrite = resolve; }));
    render(<FileManager />);

    await screen.findByRole("button", { name: "README.md" });
    fireEvent.click(screen.getByRole("button", { name: "README.md" }));
    const editor = await screen.findByLabelText("文件内容");
    await waitFor(() => expect(editor).toBeEnabled());
    fireEvent.change(editor, { target: { value: "saved contents" } });
    fireEvent.click(screen.getByRole("button", { name: "保存" }));

    await waitFor(() => expect(writeFile).toHaveBeenCalledWith(path, "saved contents"));
    expect(editor).toBeDisabled();
    expect(screen.getByRole("button", { name: "README.md" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "重命名" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "删除" })).toBeDisabled();
    expect(screen.getByLabelText(/单文件最大 100 MB/)).toBeDisabled();
    expect(screen.getByLabelText("当前路径")).toBeDisabled();
    expect(screen.getByRole("button", { name: "上级目录" })).toBeDisabled();
    expect(screen.getByLabelText("显示隐藏文件")).toBeDisabled();
    expect(screen.getByRole("button", { name: "刷新" })).toBeDisabled();
    expect(screen.getByLabelText("新建目录")).toBeDisabled();
    expect(screen.getByRole("button", { name: "创建" })).toBeDisabled();
    expect(screen.getAllByRole("button", { name: "打开" }).every((button) => (button as HTMLButtonElement).disabled)).toBe(true);
    await userEvent.setup().type(editor, " changed during save");
    expect(editor).toHaveValue("saved contents");

    await act(async () => finishWrite());
    expect(await screen.findByText("文件已保存")).toBeInTheDocument();
    expect(writeFile).toHaveBeenCalledExactlyOnceWith(path, "saved contents");
  });

  it("ignores a stale refresh result after navigating to another directory", async () => {
    let finishRefresh!: (entries: Awaited<ReturnType<typeof listFiles>>) => void;
    const refreshResult = new Promise<Awaited<ReturnType<typeof listFiles>>>((resolve) => {
      finishRefresh = resolve;
    });
    vi.mocked(listFiles)
      .mockResolvedValueOnce([{
        name: "folder",
        path: "/home/gem/workspace/folder",
        is_directory: true,
      }])
      .mockReturnValueOnce(refreshResult)
      .mockResolvedValueOnce([]);
    render(<FileManager />);

    await screen.findByRole("row", { name: /folder/ });
    fireEvent.click(screen.getByRole("button", { name: "刷新" }));
    await waitFor(() => expect(listFiles).toHaveBeenCalledTimes(2));

    fireEvent.change(screen.getByLabelText("当前路径"), {
      target: { value: "/home/gem/workspace/folder" },
    });
    fireEvent.click(screen.getByRole("button", { name: "打开" }));
    await waitFor(() => expect(listFiles).toHaveBeenCalledTimes(3));
    expect(await screen.findByText("此目录为空")).toBeInTheDocument();

    await act(async () => {
      finishRefresh([{
        name: "stale.md",
        path: "/home/gem/workspace/stale.md",
        is_directory: false,
      }]);
    });
    expect(screen.queryByRole("button", { name: "stale.md" })).not.toBeInTheDocument();
  });

  it("does not refresh the old directory after a mutation finishes following navigation", async () => {
    let finishCreate!: () => void;
    vi.mocked(runShell).mockReturnValue(new Promise<void>((resolve) => { finishCreate = resolve; }));
    vi.mocked(listFiles)
      .mockResolvedValueOnce([{
        name: "folder",
        path: "/home/gem/workspace/folder",
        is_directory: true,
      }])
      .mockResolvedValueOnce([]);
    render(<FileManager />);

    await screen.findByRole("button", { name: "folder" });
    fireEvent.change(screen.getByLabelText("新建目录"), { target: { value: "new-folder" } });
    fireEvent.click(screen.getByRole("button", { name: "创建" }));
    await waitFor(() => expect(runShell).toHaveBeenCalled());

    fireEvent.change(screen.getByLabelText("当前路径"), {
      target: { value: "/home/gem/workspace/folder" },
    });
    fireEvent.submit(screen.getByLabelText("当前路径").closest("form")!);
    await waitFor(() => expect(listFiles).toHaveBeenCalledTimes(2));
    expect(await screen.findByText("此目录为空")).toBeInTheDocument();

    await act(async () => finishCreate());
    expect(await screen.findByText("目录已创建")).toBeInTheDocument();
    expect(listFiles).toHaveBeenCalledTimes(2);
  });
});
