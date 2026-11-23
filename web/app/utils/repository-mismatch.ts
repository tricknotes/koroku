// A deployment fetches only from the repository the app was set up with
// (ARCHITECTURE 5). The fetch step refuses a name that denotes a
// repository with another ID, and koroku writes why in English; this reads
// that reason for the console.

export interface RepositoryMismatch {
  /** The repository name the app fetches by. */
  name: string;
  /** The ID of the repository GitHub has under that name now. */
  found: string;
  /** The ID of the repository the app was set up with. */
  expected: string;
}

const MISMATCH =
  /(\S+) is not the repository this app was set up with: GitHub has repository (\d+) under that name, the app was set up with repository (\d+)/;

/**
 * The repository a deployment refused because its name denotes another
 * one; null for any other error.
 */
export function repositoryMismatch(
  error: string | null | undefined,
): RepositoryMismatch | null {
  const match = MISMATCH.exec(error ?? '');
  if (!match) return null;
  const [, name = '', found = '', expected = ''] = match;
  return { name, found, expected };
}

/** The headline of such a failure, in the reader's words. */
export function repositoryMismatchTitle(mismatch: RepositoryMismatch): string {
  return `「${mismatch.name}」が、アプリに設定したものとは別のリポジトリを指しているため、デプロイできません`;
}
