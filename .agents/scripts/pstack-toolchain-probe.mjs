import { spawnSync } from "node:child_process";

function run(command, args, input) {
  const result = spawnSync(command, args, {
    input,
    encoding: "utf8",
    timeout: 4000,
    maxBuffer: 1024 * 1024,
    env:
      command === "go" && (args[0] === "mod" || args[0] === "-C")
        ? {
            ...process.env,
            GOENV: "off",
            GOFLAGS: "",
            GOTOOLCHAIN: "local",
            GOPROXY: "off",
            GOSUMDB: "off",
            GOPRIVATE: "",
            GONOPROXY: "",
            GOWORK: "off",
          }
        : process.env,
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
  if (settings.GOTOOLCHAIN !== "local")
    throw new Error(
      `GOTOOLCHAIN must be local; received ${JSON.stringify(settings.GOTOOLCHAIN)}`,
    );
  const expectedGo = JSON.parse(
    run("go", ["mod", "edit", "-json", "scripts/go.mod"]),
  ).Toolchain;
  if (typeof expectedGo !== "string" || !expectedGo)
    throw new Error("missing toolchain directive in scripts/go.mod");
  for (const [name, expected] of Object.entries({
    GOVERSION: expectedGo,
    CGO_ENABLED: "1",
  })) {
    if (settings[name] !== expected)
      throw new Error(
        `${name} must be ${expected}; received ${JSON.stringify(settings[name])}`,
      );
  }
  run("go", ["-C", "scripts", "list", "-mod=readonly", "-m"]);
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
