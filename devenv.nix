{ pkgs, ... }:
let
  pigRelease = {
    "x86_64-linux" = {
      archive = "pig-0.3.1-linux-amd64.tar.gz";
      root = "pig-0.3.1-linux-amd64";
      hash = "sha256-HKuVtN4SI7qqKBJdBm1MyA/vIfnr6+Fq2AFPxCFXCXw=";
    };
    "aarch64-linux" = {
      archive = "pig-0.3.1-linux-arm64.tar.gz";
      root = "pig-0.3.1-linux-arm64";
      hash = "sha256-aD8wY3ocuU5uXYMENHwmEYuaMO/nQekOr++qDOCeUoE=";
    };
    "x86_64-darwin" = {
      archive = "pig-0.3.1-darwin-amd64.tar.gz";
      root = "pig-0.3.1-darwin-amd64";
      hash = "sha256-5/As6YpaT8KOE4jSfxsIAm+XejlTEip0a4yShlXMc8E=";
    };
    "aarch64-darwin" = {
      archive = "pig-0.3.1-darwin-arm64.tar.gz";
      root = "pig-0.3.1-darwin-arm64";
      hash = "sha256-82ywiM/qkhh8LXIqiPnMEuc14SpizU6LpUFnWnni3MA=";
    };
  };
  system = pkgs.stdenv.hostPlatform.system;
  release = pigRelease.${system} or (throw "PiG 0.3.1 has no release archive for ${system}");
  pig = pkgs.stdenvNoCC.mkDerivation {
    pname = "pig";
    version = "0.3.1";
    src = pkgs.fetchurl {
      url = "https://github.com/MichaelKinsy/PiG/releases/download/v0.3.1/${release.archive}";
      hash = release.hash;
    };
    sourceRoot = release.root;
    dontConfigure = true;
    dontBuild = true;
    installPhase = ''
      install -Dm755 pig "$out/bin/pig"
      install -Dm644 LICENSE "$out/share/licenses/pig/LICENSE"
      install -Dm644 NOTICE "$out/share/doc/pig/NOTICE"
      install -Dm644 THIRD_PARTY_NOTICES.md "$out/share/doc/pig/THIRD_PARTY_NOTICES.md"
      cp -R LICENSES "$out/share/licenses/pig/"
    '';
    meta = {
      description = "PiG terminal coding agent";
      homepage = "https://github.com/MichaelKinsy/PiG";
      mainProgram = "pig";
      platforms = builtins.attrNames pigRelease;
    };
  };
in
{
  languages.go = {
    enable = true;
    package = pkgs.go_1_27;
  };

  env.GOTOOLCHAIN = "local";
  packages = [ pkgs.git pkgs.bun pig pkgs.jq ];

  scripts.check-pig-extension.exec = ''
    bash "$DEVENV_ROOT/scripts/check-pig-extension.sh"
  '';

  enterTest = "check-pig-extension";
}
