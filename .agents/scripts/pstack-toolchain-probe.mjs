import { spawnSync } from "node:child_process";

function run(command, args, input) {
  const result = spawnSync(command, args, {
    input,
    encoding: "utf8",
    timeout: 4000,
    maxBuffer: 4096,
  });
  if (result.error || result.status !== 0)
    throw new Error(
      `${command} failed: ${result.error?.message || result.stderr || `exit ${result.status}`}`,
    );
  return result.stdout;
}

try {
  const settings = JSON.parse(
    run("go", [
      "env",
      "-json",
      "GOVERSION",
      "CGO_ENABLED",
      "GOTOOLCHAIN",
      "CC",
    ]),
  );
  for (const [name, expected] of Object.entries({
    GOVERSION: "go1.27.1",
    CGO_ENABLED: "1",
    GOTOOLCHAIN: "local",
  })) {
    if (settings[name] !== expected)
      throw new Error(
        `${name} must be ${expected}; received ${JSON.stringify(settings[name])}`,
      );
  }
  if (typeof settings.CC !== "string" || !settings.CC)
    throw new Error("Go has no configured C compiler");
  run(
    settings.CC,
    ["-fsyntax-only", "-x", "c", "-"],
    '#include <stdio.h>\n#include <stdlib.h>\nint main(void) { return puts("probe") < 0 ? EXIT_FAILURE : EXIT_SUCCESS; }\n',
  );
  console.log(
    JSON.stringify({
      go: settings.GOVERSION,
      cgo: settings.CGO_ENABLED,
      toolchain: settings.GOTOOLCHAIN,
      compiler: "syntax-and-headers-verified",
    }),
  );
} catch (error) {
  console.error(`Project toolchain preflight: ${error.message}`);
  process.exitCode = 1;
}
