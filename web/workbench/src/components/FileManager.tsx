import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type FormEvent } from 'react';
import { downloadFile, listFiles, MAX_UPLOAD_BYTES, readFile, uploadFile, writeFile, type AioFile } from '../app/fileApi';
import { runShell } from '../app/aio';
import { shellQuote } from '../util/shellQuote';

const INITIAL_PATH = '/home/gem/workspace';

function parentPath(path: string): string {
  const trimmed = path.replace(/\/+$/, '');
  const index = trimmed.lastIndexOf('/');
  return index <= 0 ? '/' : trimmed.slice(0, index);
}

function joinPath(directory: string, name: string): string {
  return `${directory.replace(/\/+$/, '')}/${name}`;
}

function validName(name: string): boolean {
  return name.trim().length > 0 && name !== '.' && name !== '..' && !name.includes('/');
}

function formatSize(size?: number | null): string {
  if (size === undefined || size === null) return '—';
  if (size < 1024) return `${size} B`;
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(1)} KB`;
  return `${(size / 1024 / 1024).toFixed(1)} MB`;
}

export default function FileManager() {
  const [path, setPath] = useState(INITIAL_PATH);
  const [pathInput, setPathInput] = useState(INITIAL_PATH);
  const [showHidden, setShowHidden] = useState(false);
  const [files, setFiles] = useState<AioFile[]>([]);
  const [selected, setSelected] = useState<AioFile | null>(null);
  const [content, setContent] = useState('');
  const [fileReady, setFileReady] = useState(false);
  const [message, setMessage] = useState('');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(true);
  const [directoryLoadFailed, setDirectoryLoadFailed] = useState(false);
  const [saving, setSaving] = useState(false);
  const [uploading, setUploading] = useState(false);
  const [directoryName, setDirectoryName] = useState('');
  const [renameName, setRenameName] = useState('');
  const [uploadProgress, setUploadProgress] = useState<number | null>(null);
  const listRequestId = useRef(0);
  const readRequestId = useRef(0);
  const uploadLock = useRef(false);
  const pathRef = useRef(path);
  const showHiddenRef = useRef(showHidden);
  useLayoutEffect(() => {
    pathRef.current = path;
    showHiddenRef.current = showHidden;
  }, [path, showHidden]);

  const loadDirectory = useCallback(async (directory: string, includeHidden: boolean): Promise<string | null> => {
    const requestId = ++listRequestId.current;
    setLoading(true);
    setDirectoryLoadFailed(false);
    setError('');
    try {
      const entries = await listFiles(directory, includeHidden);
      if (requestId === listRequestId.current) {
        setFiles([...entries].sort((a, b) => Number(b.is_directory) - Number(a.is_directory) || a.name.localeCompare(b.name)));
        setDirectoryLoadFailed(false);
      }
      return null;
    } catch (reason) {
      const message = reason instanceof Error ? reason.message : '读取目录失败';
      if (requestId === listRequestId.current) {
        setFiles([]);
        setDirectoryLoadFailed(true);
        setError(message);
        return message;
      }
      return null;
    } finally {
      if (requestId === listRequestId.current) setLoading(false);
    }
  }, []);

  async function refreshCurrentDirectory(directory: string, includeHidden: boolean) {
    if (pathRef.current !== directory || showHiddenRef.current !== includeHidden) return null;
    return loadDirectory(directory, includeHidden);
  }

  useEffect(() => {
    let current = true;
    const requestId = ++listRequestId.current;
    listFiles(path, showHidden)
      .then((entries) => {
        if (!current || requestId !== listRequestId.current) return;
        setFiles([...entries].sort((a, b) => Number(b.is_directory) - Number(a.is_directory) || a.name.localeCompare(b.name)));
        setDirectoryLoadFailed(false);
        setError('');
      })
      .catch((reason: unknown) => {
        if (!current || requestId !== listRequestId.current) return;
        setFiles([]);
        setDirectoryLoadFailed(true);
        setError(reason instanceof Error ? reason.message : '读取目录失败');
      })
      .finally(() => {
        if (current && requestId === listRequestId.current) setLoading(false);
      });
    return () => {
      current = false;
    };
  }, [path, showHidden]);

  const selectedPath = useMemo(() => selected?.path || '', [selected]);

  async function selectEntry(entry: AioFile) {
    const requestId = ++readRequestId.current;
    setSelected(entry);
    setRenameName(entry.name);
    setMessage('');
    setContent('');
    setFileReady(false);
    if (entry.is_directory) return;
    setError('');
    try {
      const nextContent = await readFile(entry.path);
      if (requestId === readRequestId.current) {
        setContent(nextContent);
        setFileReady(true);
      }
    } catch (reason) {
      if (requestId === readRequestId.current) setError(reason instanceof Error ? reason.message : '读取文件失败');
    }
  }

  async function save() {
    if (!selected || selected.is_directory || uploading) return;
    setSaving(true);
    setError('');
    try {
      await writeFile(selected.path, content);
      setMessage('文件已保存');
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '保存文件失败');
    } finally {
      setSaving(false);
    }
  }

  async function download(entry: AioFile) {
    setError('');
    try {
      await downloadFile(entry.path, entry.name);
      setMessage(`${entry.name} 下载已开始`);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '下载失败');
    }
  }

  async function createDirectory(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (saving || uploading) return;
    if (!validName(directoryName)) {
      setError('请输入有效的目录名称（不能包含 /）');
      return;
    }
    setError('');
    try {
      await runShell(`mkdir -- ${shellQuote(joinPath(path, directoryName))}`, path);
      setMessage('目录已创建');
      setDirectoryName('');
      await refreshCurrentDirectory(path, showHidden);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '创建目录失败');
    }
  }

  async function rename(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (saving || uploading) return;
    if (!selected || !validName(renameName)) {
      setError('请输入有效的新名称（不能包含 /）');
      return;
    }
    const nextPath = joinPath(path, renameName);
    if (nextPath === selected.path) return;
    if (files.some((entry) => entry.path === nextPath)) {
      setError('目标名称已存在，请选择其他名称');
      return;
    }
    setError('');
    try {
      await runShell(`mv -n -- ${shellQuote(selected.path)} ${shellQuote(nextPath)} && test ! -e ${shellQuote(selected.path)}`, path);
      setMessage('名称已更新');
      setSelected(null);
      setContent('');
      await refreshCurrentDirectory(path, showHidden);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '重命名失败');
    }
  }

  async function remove() {
    if (!selected || saving || uploading) return;
    if (!window.confirm(`确定删除“${selected.name}”吗？此操作不可撤销。`)) return;
    setError('');
    try {
      await runShell(`rm -r -- ${shellQuote(selected.path)}`, path);
      setMessage('已删除');
      setSelected(null);
      setContent('');
      await refreshCurrentDirectory(path, showHidden);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : '删除失败');
    }
  }

  async function upload(filesToUpload: FileList | File[]) {
    if (saving || uploadLock.current) return;
    uploadLock.current = true;
    setUploading(true);
    try {
      const queue = Array.from(filesToUpload);
      const errors: string[] = [];
      const existingPaths = new Map<string, boolean>();
      let checkedDirectory = false;
      for (const file of queue) {
        if (file.size > MAX_UPLOAD_BYTES) {
          errors.push(`${file.name} 超过 100 MB，未上传`);
          continue;
        }
        if (!checkedDirectory) {
          checkedDirectory = true;
          try {
            (await listFiles(path, true)).forEach((entry) => existingPaths.set(entry.path, entry.is_directory));
          } catch (reason) {
            const details = reason instanceof Error ? reason.message : '读取目录失败';
            errors.push(`${file.name}：无法确认目标文件是否已存在，未上传（${details}）`);
            break;
          }
        }
        const destination = joinPath(path, file.name);
        const existingIsDirectory = existingPaths.get(destination);
        if (existingIsDirectory === true) {
          errors.push(`${file.name} 与现有目录同名，未上传`);
          continue;
        }
        if (existingIsDirectory === false && !window.confirm(`“${file.name}”已存在，确定覆盖吗？`)) {
          continue;
        }
        setError('');
        setUploadProgress(0);
        try {
          await uploadFile(file, destination, setUploadProgress);
          existingPaths.set(destination, false);
          if (selected?.path === destination) {
            readRequestId.current += 1;
            setSelected(null);
            setContent('');
            setFileReady(false);
          }
          setMessage(`${file.name} 上传完成`);
        } catch (reason) {
          errors.push(reason instanceof Error ? reason.message : `${file.name} 上传失败`);
        } finally {
          setUploadProgress(null);
        }
      }
      const listError = await refreshCurrentDirectory(path, showHidden);
      if (listError) errors.push(listError);
      if (errors.length) setError(errors.join('\n'));
    } finally {
      uploadLock.current = false;
      setUploading(false);
      setUploadProgress(null);
    }
  }

  return (
    <section className="file-manager" aria-label="文件管理">
      <div className="file-toolbar">
        <button className="button button-secondary" type="button" onClick={() => { if (saving || uploading) return; const next = parentPath(path); setSelected(null); setContent(''); setLoading(true); setPath(next); setPathInput(next); }} disabled={path === '/' || saving || uploading}>
          上级目录
        </button>
        <form className="path-form" onSubmit={(event) => {
          event.preventDefault();
          const next = pathInput.trim();
          if (saving || uploading) return;
          if (!next) { setError('请输入目录路径'); return; }
          setSelected(null);
          setContent('');
          setLoading(true);
          setPath(next);
        }}>
          <label className="field-label" htmlFor="file-path">当前路径</label>
          <input id="file-path" value={pathInput} onChange={(event) => setPathInput(event.target.value)} disabled={saving || uploading} />
          <button className="button button-secondary" type="submit" disabled={saving || uploading}>打开</button>
        </form>
        <label className="checkbox-label">
          <input type="checkbox" checked={showHidden} onChange={(event) => { setLoading(true); setShowHidden(event.target.checked); }} disabled={saving || uploading} />
          显示隐藏文件
        </label>
        <button className="button button-secondary" type="button" onClick={() => { setLoading(true); void loadDirectory(path, showHidden); }} disabled={loading || saving || uploading}>
          {directoryLoadFailed ? '重试' : '刷新'}
        </button>
      </div>

      <div
        className="upload-dropzone"
        onDragOver={(event) => event.preventDefault()}
        onDrop={(event) => { event.preventDefault(); if (!saving && !uploadLock.current) void upload(event.dataTransfer.files); }}
      >
        <input id="file-upload" type="file" multiple onChange={(event) => {
          if (event.target.files?.length) void upload(event.target.files);
          event.currentTarget.value = '';
        }} disabled={saving || uploading} />
        <label className="button button-secondary upload-picker" htmlFor="file-upload">{uploading ? '上传中…' : '拖拽文件到此处，或选择文件上传（单文件最大 100 MB）'}</label>
        {uploadProgress !== null && (
          <div className="upload-progress">
            <progress aria-label="文件上传进度" max={100} value={uploadProgress} />
            <span>{uploadProgress}%</span>
          </div>
        )}
      </div>

      <div className="file-actions">
        <form className="inline-form" onSubmit={createDirectory}>
          <label className="field-label" htmlFor="new-directory-name">新建目录</label>
          <input id="new-directory-name" value={directoryName} onChange={(event) => setDirectoryName(event.target.value)} placeholder="目录名称" disabled={saving || uploading} />
          <button className="button button-secondary" type="submit" disabled={saving || uploading}>创建</button>
        </form>
        {selected && (
          <form className="inline-form" onSubmit={rename}>
            <label className="field-label" htmlFor="rename-entry">重命名所选项</label>
            <input id="rename-entry" value={renameName} onChange={(event) => setRenameName(event.target.value)} disabled={saving || uploading} />
            <button className="button button-secondary" type="submit" disabled={saving || uploading}>重命名</button>
            <button className="button button-danger" type="button" onClick={() => void remove()} disabled={saving || uploading}>删除</button>
          </form>
        )}
      </div>

      {message && <p className="success-text" role="status">{message}</p>}
      {error && <p className="error-text" role="alert">{error}</p>}

      <div className="file-layout">
        <div className="file-list-wrap">
          {loading ? <p role="status">正在读取目录…</p> : (
            <table className="file-table">
              <thead><tr><th scope="col">名称</th><th scope="col">类型</th><th scope="col">大小</th><th scope="col">操作</th></tr></thead>
              <tbody>
                {files.map((entry) => (
                  <tr key={entry.path} className={selectedPath === entry.path ? 'selected-row' : ''}>
                    <td>
                      <button className="file-entry" type="button" onClick={() => void selectEntry(entry)} disabled={saving || uploading}>
                        <span aria-hidden="true">{entry.is_directory ? '📁' : '📄'}</span> {entry.name}
                      </button>
                    </td>
                    <td>{entry.is_directory ? '目录' : '文件'}</td>
                    <td>{entry.is_directory ? '—' : formatSize(entry.size)}</td>
                    <td>{entry.is_directory ? (
                      <button className="download-link" type="button" disabled={saving || uploading} onClick={() => {
                        setSelected(null);
                        setContent('');
                        setLoading(true);
                        setPath(entry.path);
                        setPathInput(entry.path);
                      }}>打开</button>
                    ) : <button className="download-link" type="button" onClick={() => void download(entry)} disabled={saving || uploading}>下载</button>}</td>
                  </tr>
                ))}
                {!files.length && !directoryLoadFailed && <tr><td colSpan={4}>此目录为空</td></tr>}
              </tbody>
            </table>
          )}
        </div>
        {selected && !selected.is_directory && (
          <section className="file-editor" aria-label={`编辑文件 ${selected.name}`}>
            <div className="editor-heading">
              <h2>{selected.name}</h2>
              <button className="button" type="button" onClick={() => void save()} disabled={saving || uploading || !fileReady}>
                {saving ? '保存中…' : '保存'}
              </button>
            </div>
            <label className="field-label" htmlFor="file-content">文件内容</label>
            <textarea id="file-content" spellCheck={false} value={content} onChange={(event) => setContent(event.target.value)} disabled={!fileReady || saving || uploading} />
          </section>
        )}
      </div>
    </section>
  );
}
