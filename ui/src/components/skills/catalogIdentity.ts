/**
 * The identity rules a catalog skill's OCI source has to satisfy.
 *
 * Three consumers ask the same two questions about an image reference — which
 * repository it names, and whether it is pinned by a digest — so the answers
 * live here once. `agentTemplateDraft` matches form rows against the catalog,
 * `catalogSelection` matches spec entries and the picker's selections against
 * it, and `runtimeConfig` validates the version pins the catalog itself offers.
 * The rules were previously written out three times and could drift.
 */

/** The digest pin a skill source must carry; the same shape the CRD's CEL accepts. */
export const OCI_DIGEST_REF = /^[^\s@]+@sha256:[0-9a-f]{64}$/;

/** The repository part of a digest-pinned reference, before the `@`. */
export function imageRepository(image: string): string {
  return image.split("@")[0];
}

/**
 * Whether two image references name the same repository, whatever their digests.
 * Every "is this the catalog's own skill?" check reduces to this: a digest is a
 * pin, not an identity, so the same repository at an older digest is still the
 * catalog's skill — saved before a chart upgrade moved the pin.
 */
export function sameImageRepository(left: string, right: string): boolean {
  return imageRepository(left) === imageRepository(right);
}
