/**
 * TypeScript client for the Ilavrita FHIR API.
 *
 * The client speaks FHIR only. It deliberately exposes no PocketBase concept,
 * so the storage backend can change without breaking callers (FR-043).
 */

export const FHIR_CONTENT_TYPE = "application/fhir+json";

export interface ClientOptions {
  /** Absolute base URL of an Ilavrita deployment, such as `https://fhir.example.org`. */
  readonly baseUrl: string;
  /** Injected for testing; defaults to the global `fetch`. */
  readonly fetch?: typeof globalThis.fetch;
}

/** A FHIR CapabilityStatement, narrowed to the fields this client relies on. */
export interface CapabilityStatement {
  readonly resourceType: "CapabilityStatement";
  readonly fhirVersion: string;
  readonly software: { readonly name: string; readonly version: string };
}

export class IlavritaClient {
  readonly #baseUrl: string;
  readonly #fetch: typeof globalThis.fetch;

  constructor(options: ClientOptions) {
    this.#baseUrl = options.baseUrl.replace(/\/+$/, "");
    this.#fetch = options.fetch ?? globalThis.fetch;
  }

  /** Reads what the deployment declares it supports. */
  async capabilities(): Promise<CapabilityStatement> {
    const response = await this.#fetch(`${this.#baseUrl}/fhir/R4/metadata`, {
      headers: { Accept: FHIR_CONTENT_TYPE },
    });

    if (!response.ok) {
      throw new Error(`Ilavrita returned ${response.status} for the CapabilityStatement`);
    }

    return (await response.json()) as CapabilityStatement;
  }
}
