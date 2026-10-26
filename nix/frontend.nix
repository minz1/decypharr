{
  lib,
  stdenvNoCC,
  nodejs,
  pnpm,
  fetchPnpmDeps,
  pnpmConfigHook,
}:

stdenvNoCC.mkDerivation (finalAttrs: {
  pname = "decypharr-frontend";
  version = "0";

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../package.json
      ../pnpm-lock.yaml
      ../pnpm-workspace.yaml
      ../vite.config.ts
      ../vitest.config.ts
      ../tsconfig.json
      ../tsconfig.node.json
      ../.oxlintrc.json
      ../.oxfmtrc.json
      ../scripts/minify-js.js
      ../pkg/server/ui
      ../pkg/server/assets/js
      ../pkg/server/templates
    ];
  };

  pnpmDeps = fetchPnpmDeps {
    inherit (finalAttrs) pname version src;
    inherit pnpm;
    fetcherVersion = 4;
    hash = "sha256-iU/+kbdMHr/1GwLLR8PEzfzLDc3GYokVxzF60g9X7go=";
  };

  nativeBuildInputs = [
    nodejs
    pnpm
    pnpmConfigHook
  ];

  buildPhase = ''
    runHook preBuild
    pnpm build
    runHook postBuild
  '';

  installPhase = ''
    runHook preInstall
    mkdir -p $out
    cp -r pkg/server/assets/build/vite pkg/server/assets/build/js $out/
    runHook postInstall
  '';
})
