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
      ../vitest.global-setup.ts
      ../tsconfig.json
      ../tsconfig.app.json
      ../tsconfig.node.json
      ../.oxlintrc.json
      ../.oxfmtrc.json
      ../scripts/minify-js.js
      ../pkg/server/ui
      ../pkg/server/templates
    ];
  };

  pnpmDeps = fetchPnpmDeps {
    inherit (finalAttrs) pname version src;
    inherit pnpm;
    fetcherVersion = 4;
    hash = "sha256-LeJfCYXbbCxAXT9EuHQSAa/R53+7pR/5IFT5brXNcRc=";
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
    cp -r pkg/server/assets/build/vite $out/
    runHook postInstall
  '';
})
