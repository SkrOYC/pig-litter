import { createHash } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';

const [source, directory] = process.argv.slice(2);
const original = await readFile(source, 'utf8');
const imports = 'import (\n\t"context"\n';
const entry = 'func (rt *Runtime) Resume(id string, opts SessionStartOptions) (*Session, error) {\n';
if (original.split(imports).length !== 2 || original.split(entry).length !== 2) {
  throw new Error('The pinned runtime no longer matches the resume probe.');
}
const probe = `\tif path := os.Getenv("P07_RESUME_TRACE"); path != "" {
\t\tfile, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
\t\tif err != nil {
\t\t\tpanic(err)
\t\t}
\t\tif _, err := fmt.Fprintln(file, id); err != nil {
\t\t\tpanic(err)
\t\t}
\t\tif err := file.Close(); err != nil {
\t\t\tpanic(err)
\t\t}
\t}
`;
const instrumented = original.replace(imports, 'import (\n\t"context"\n\t"os"\n').replace(entry, entry + probe);
const target = join(directory, 'p07-runtime.go');
await writeFile(target, instrumented);
await writeFile(join(directory, 'p07-overlay.json'), JSON.stringify({ Replace: { [source]: target } }));
const digest = value => createHash('sha256').update(value).digest('hex');
process.stdout.write(JSON.stringify({ source, sourceSHA256: digest(original), instrumentedSHA256: digest(instrumented) }) + '\n');
