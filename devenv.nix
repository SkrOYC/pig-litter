{ pkgs, inputs, config, ... }:
let
  go = pkgs.go_1_27.overrideAttrs (_: {
    version = "1.27.4";
    src = inputs.go-src;
  });
  pigRelease = {
    "x86_64-linux" = {
      archive = "pig-0.7.0-linux-amd64.tar.gz";
      root = "pig-0.7.0-linux-amd64";
      hash = "sha256-C1q3y19oDn0zU72qOhAYhjw8NVL4i+xzA3BmjlgZmVo=";
    };
    "aarch64-linux" = {
      archive = "pig-0.7.0-linux-arm64.tar.gz";
      root = "pig-0.7.0-linux-arm64";
      hash = "sha256-iNLctKSFKpgNwcqSAu7ZRV/4+wEVU8jbYsG4XGdqNms=";
    };
    "x86_64-darwin" = {
      archive = "pig-0.7.0-darwin-amd64.tar.gz";
      root = "pig-0.7.0-darwin-amd64";
      hash = "sha256-bHRLOxNo+4Jy36ZAM/BsAgeCLe3gXxrA34OOlg1+OsE=";
    };
    "aarch64-darwin" = {
      archive = "pig-0.7.0-darwin-arm64.tar.gz";
      root = "pig-0.7.0-darwin-arm64";
      hash = "sha256-2Hxl4DrOx3yPo87TI8J/Jzhrab3sqA/eVgCMhdR60aQ=";
    };
  };
  system = pkgs.stdenv.hostPlatform.system;
  release = pigRelease.${system} or (throw "PiG 0.7.0 has no release archive for ${system}");
  stockPig = pkgs.stdenvNoCC.mkDerivation {
    pname = "pig";
    version = "0.7.0";
    src = pkgs.fetchurl {
      url = "https://github.com/MichaelKinsy/PiG/releases/download/v0.7.0/${release.archive}";
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
  pigSource = builtins.fromJSON (builtins.readFile ./pig-source.json);
  pigSourceDigest = builtins.hashFile "sha256" ./pig-source.json;
  pigSourcePatch = ./. + "/${pigSource.patch.path}";
  pigSourceArchive = pkgs.fetchurl {
    url = pigSource.archive.url;
    sha256 = pigSource.archive.sha256;
  };
  patchedPigSource =
    assert builtins.hashFile "sha256" pigSourcePatch == pigSource.patch.sha256;
    pkgs.runCommand "pig-${pigSource.version}" {
      nativeBuildInputs = [ pkgs.gitMinimal pkgs.gnutar pkgs.gzip ];
    } ''
      tar -xzf ${pigSourceArchive}
      cd ${pkgs.lib.escapeShellArg pigSource.archive.root}
      git apply --check ${pigSourcePatch}
      git apply ${pigSourcePatch}
      mkdir -p "$out"
      cp -R . "$out/"
    '';
  pigPreview =
    let
      patchDigest = pigSource.patch.sha256;
      build = "pig-litter-patch-${patchDigest}";
      binaryDigest = "19b6546c283370cdf51314d81686bd2fb802e9db209651a1812fc4c40101e412";
      buildGo = pkgs.buildGo127Module.override { inherit go; };
    in
    {
      inherit build binaryDigest;
      package = buildGo {
        pname = "pig-litter-host-preview";
        version = pigSource.version;
        src = patchedPigSource;
        sourceRoot = pigSource.archive.root;
        proxyVendor = true;
        vendorHash = "sha256-Wa2wOKBr760duMx01Ln5C2Pyw3XXekcVNuS3xpTsqW0=";
        subPackages = [ "cmd/pig" ];
        nativeBuildInputs = [ pkgs.gitMinimal ];
        env.CGO_ENABLED = "0";
        modBuildPhase = ''
          runHook preBuild
          export GIT_SSL_CAINFO="$NIX_SSL_CERT_FILE"
          for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
            GOOS="''${target%/*}" GOARCH="''${target#*/}" CGO_ENABLED=0 go list -deps ./cmd/pig > /dev/null
          done
          runHook postBuild
        '';
        ldflags = [ "-s" "-w" "-X github.com/MichaelKinsy/PiG/coding/cli.Build=${build}" ];
        doCheck = false;
        postInstall = ''
          install -Dm644 -t "$out/share/licenses/pig" LICENSE NOTICE THIRD_PARTY_NOTICES.md
          cp -r LICENSES "$out/share/licenses/pig/LICENSES"
          install -Dm644 ${./pig-source.json} "$out/share/pig/source.json"
        '';
      };
    };
  pig = if system == "x86_64-linux" then pigPreview.package else stockPig;
in
{
  languages.go = {
    enable = true;
    package = go;
  };

  env = {
    GOTOOLCHAIN = "local";
    CGO_ENABLED = "1";
  } // pkgs.lib.optionalAttrs (system == "x86_64-linux") {
    PIG_LITTER_EXPECTED_PIG = "${pigPreview.package}/bin/pig";
    PIG_LITTER_EXPECTED_PIG_SHA256 = pigPreview.binaryDigest;
    PIG_LITTER_EXPECTED_PIG_BUILD = pigPreview.build;
    PIG_LITTER_SOURCE_DIR = "${patchedPigSource}";
    PIG_LITTER_SDK_DIR = "${patchedPigSource}/extensions/sdk";
    GOWORK = "${config.devenv.state}/pig-sdk/${pigSourceDigest}/go.work";
  };
  packages = [ pkgs.git pkgs.tmux pig pkgs.jq ];

  enterShell = pkgs.lib.optionalString (system == "x86_64-linux") ''
    cmp "$DEVENV_ROOT/pig-source.json" "${pigPreview.package}/share/pig/source.json" || exit 1
    PIG_LITTER_WORKSPACE_BRIDGE="$DEVENV_ROOT/extensions/go.work" bash "$DEVENV_ROOT/scripts/prepare-pig-workspace.sh" "$PIG_LITTER_SOURCE_DIR" "$(dirname "$GOWORK")" "$DEVENV_ROOT/scripts" "$DEVENV_ROOT/extensions/pig-litter" > /dev/null || exit 1
  '';

  scripts.check-pig-extension.exec = ''
    bash "$DEVENV_ROOT/scripts/check-pig-extension.sh" "$@"
  '';

  enterTest = "check-pig-extension";
}
