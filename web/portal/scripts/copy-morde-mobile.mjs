import { cpSync, copyFileSync, existsSync, mkdirSync, readFileSync, rmSync, statSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const legalFiles = ['LICENSE', 'NOTICE', 'THIRD_PARTY_NOTICES.md'];

export function copyMordeMobileWeb(webDist, outputDir, legalDir) {
  const indexPath = join(webDist, 'index.html');
  if (!existsSync(indexPath) || !statSync(indexPath).isFile()) throw new Error('Morde mobile Web build is missing index.html');
  if (!readFileSync(indexPath, 'utf8').includes('/app/')) throw new Error('Morde mobile Web build does not use the /app/ base path');
  for (const file of legalFiles) {
    const path = join(legalDir, file);
    if (!existsSync(path) || !statSync(path).isFile()) throw new Error(`Morde mobile Web legal file is missing: ${file}`);
  }

  rmSync(outputDir, { recursive: true, force: true });
  mkdirSync(dirname(outputDir), { recursive: true });
  cpSync(webDist, outputDir, { recursive: true });
  const legalOutput = join(outputDir, 'legal');
  mkdirSync(legalOutput, { recursive: true });
  for (const file of legalFiles) copyFileSync(join(legalDir, file), join(legalOutput, file));
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), '../../..');
  copyMordeMobileWeb(
    join(repoRoot, 'project/nanoMuse/web/dist'),
    join(repoRoot, 'web/portal/dist/app'),
    join(repoRoot, 'project/nanoMuse'),
  );
  process.stdout.write('Copied Morde mobile Web and legal notices to web/portal/dist/app\n');
}
