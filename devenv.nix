{ pkgs, ... }:
let
  pigRelease = {
    "x86_64-linux" = {
      archive = "pig-0.4.1-linux-amd64.tar.gz";
      root = "pig-0.4.1-linux-amd64";
      hash = "sha256-O03jWHy/thfe9IPsYt3c81gnrMNDaxRUzFaUwi3Nrz8=";
    };
    "aarch64-linux" = {
      archive = "pig-0.4.1-linux-arm64.tar.gz";
      root = "pig-0.4.1-linux-arm64";
      hash = "sha256-4XaLv84pt4TXYAHrR+vZifBe0EL76BrYYpgW4YH6Wsg=";
    };
    "x86_64-darwin" = {
      archive = "pig-0.4.1-darwin-amd64.tar.gz";
      root = "pig-0.4.1-darwin-amd64";
      hash = "sha256-nynvASzTQHSsAq1KSub3uT1CtP69NaoIrfR3ApRNOoc=";
    };
    "aarch64-darwin" = {
      archive = "pig-0.4.1-darwin-arm64.tar.gz";
      root = "pig-0.4.1-darwin-arm64";
      hash = "sha256-JaHiveaF8oaE5JWyUNzK17KWCoQEVNgMiMNdXHqMWWY=";
    };
  };
  system = pkgs.stdenv.hostPlatform.system;
  release = pigRelease.${system} or (throw "PiG 0.4.1 has no release archive for ${system}");
  pig = pkgs.stdenvNoCC.mkDerivation {
    pname = "pig";
    version = "0.4.1";
    src = pkgs.fetchurl {
      url = "https://github.com/MichaelKinsy/PiG/releases/download/v0.4.1/${release.archive}";
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
  env.CGO_ENABLED = "1";
  packages = [ pkgs.git pkgs.tmux pig pkgs.jq ];

  scripts.check-pig-extension.exec = ''
    bash "$DEVENV_ROOT/scripts/check-pig-extension.sh"
  '';

  enterTest = "check-pig-extension";
}
