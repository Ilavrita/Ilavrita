# @ilavrita/tsconfig

Shared TypeScript configuration for Ilavrita packages.

```json
{
  "extends": "@ilavrita/tsconfig/base.json",
  "compilerOptions": { "rootDir": "src", "outDir": "dist" },
  "include": ["src"]
}
```

`base.json` targets ES2022 with `NodeNext` resolution and strict checking,
including `noUncheckedIndexedAccess` and `verbatimModuleSyntax`.

Published so that [`@ilavrita/sdk`](https://www.npmjs.com/package/@ilavrita/sdk)
resolves it as a real dependency rather than a workspace reference.
