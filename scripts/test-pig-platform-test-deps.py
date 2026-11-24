#!/usr/bin/env python3
import base64
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
import zipfile


def module_hash(files):
    lines = b"".join(hashlib.sha256(content).hexdigest().encode() + b"  " + name.encode() + b"\n" for name, content in sorted(files.items()))
    return "h1:" + base64.b64encode(hashlib.sha256(lines).digest()).decode()


class TestDependencyPreparationTests(unittest.TestCase):
    def test_test_only_dependency_is_prefetched_before_offline_replay(self):
        script = Path(__file__).with_name("verify-pig-platforms.sh").read_text()
        before, after = script.split("export GOPROXY=off", 1)
        command = 'go -C "$root_dir/extensions/pig-litter" list -mod=readonly -buildvcs=false -deps -test -f'
        self.assertEqual(before.count(command), 1)
        self.assertEqual(after.count(command), 1)
        self.assertIn('env -json GOPROXY GOWORK > "$evidence_dir/extension-test-dependency-env.json"', after)
        self.assertIn('cmp -s "$evidence_dir/extension-test-dependencies.txt" "$evidence_dir/extension-test-dependencies-offline.txt" || fail_check "$LINENO"', after)
        with tempfile.TemporaryDirectory(prefix="pig-test-deps.") as directory:
            root = Path(directory)
            extension = root / "extension"
            sdk = root / "sdk"
            proxy = root / "proxy/example.test/testonly/@v"
            for path in (extension, sdk, proxy, root / "tmp"):
                path.mkdir(parents=True)
            module = b"module example.test/testonly\n\ngo 1.27.4\n"
            source = b"package testonly\n"
            prefix = "example.test/testonly@v1.0.0/"
            files = {prefix + "go.mod": module, prefix + "testonly.go": source}
            (proxy / "v1.0.0.mod").write_bytes(module)
            (proxy / "v1.0.0.info").write_text('{"Version":"v1.0.0","Time":"2020-01-01T00:00:00Z"}')
            with zipfile.ZipFile(proxy / "v1.0.0.zip", "w") as archive:
                for name, content in files.items():
                    archive.writestr(name, content)
            (sdk / "go.mod").write_text("module example.test/sdk\n\ngo 1.27.4\n")
            (sdk / "sdk.go").write_text("package sdk\n")
            (extension / "go.mod").write_text("module example.test/extension\n\ngo 1.27.4\n\nrequire example.test/testonly v1.0.0\n")
            (extension / "go.sum").write_text(f"example.test/testonly v1.0.0 {module_hash(files)}\nexample.test/testonly v1.0.0/go.mod {module_hash({'go.mod': module})}\n")
            (extension / "main.go").write_text('package extension\nimport _ "example.test/sdk"\n')
            (extension / "main_test.go").write_text('package extension\nimport _ "example.test/testonly"\n')
            work = root / "go.work"
            work.write_text("go 1.27.4\n\nuse (\n ./extension\n ./sdk\n)\n")
            inputs = {path: path.read_bytes() for path in (work, extension / "go.mod", extension / "go.sum", sdk / "go.mod")}
            env = os.environ | {"GOWORK": str(work), "GOMODCACHE": str(root / "modules"), "GOCACHE": str(root / "build"), "GOPATH": str(root / "gopath"), "TMPDIR": str(root / "tmp"), "GOENV": "off", "GOTOOLCHAIN": "local", "GOSUMDB": "off", "GOTELEMETRY": "off", "GOFLAGS": ""}
            template = "{{if .Module}}{{.ImportPath}} {{.Module.Path}} {{.Module.Version}} {{.Module.Dir}}{{end}}"
            argv = ["go", "-C", str(extension), "list", "-mod=readonly", "-buildvcs=false", "-deps", "-test", "-f", template, "."]
            missing = subprocess.run(argv, env=env | {"GOPROXY": "off"}, capture_output=True, text=True)
            self.assertNotEqual(missing.returncode, 0)
            self.assertIn("module lookup disabled by GOPROXY=off", missing.stderr)
            online = subprocess.run(argv, env=env | {"GOPROXY": (root / "proxy").as_uri()}, capture_output=True, text=True)
            self.assertEqual(online.returncode, 0, online.stderr)
            self.assertIn("example.test/testonly example.test/testonly v1.0.0 ", online.stdout)
            self.assertIn("example.test/sdk example.test/sdk  " + str(sdk), online.stdout)
            offline = subprocess.run(argv, env=env | {"GOPROXY": "off"}, capture_output=True, text=True)
            self.assertEqual(offline.returncode, 0, offline.stderr)
            self.assertEqual(offline.stdout, online.stdout)
            self.assertEqual({path: path.read_bytes() for path in inputs}, inputs)


if __name__ == "__main__":
    unittest.main()
