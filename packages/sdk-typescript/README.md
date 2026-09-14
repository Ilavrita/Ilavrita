# @ilavrita/sdk

TypeScript client for the Ilavrita FHIR API.

Only `capabilities()` exists today. Resource, search and transaction methods
arrive as the matching server interactions ship, so the SDK never offers a call
the server cannot answer.

```ts
import { IlavritaClient } from "@ilavrita/sdk";

const client = new IlavritaClient({ baseUrl: "http://127.0.0.1:8090" });
console.log(await client.capabilities());
```
