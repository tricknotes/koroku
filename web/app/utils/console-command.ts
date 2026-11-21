/**
 * Characters the host shell would act on (expansion, pipes, redirection,
 * globbing, comments, escapes). A command with any of them is passed to
 * `sh -c` in the container instead (ARCHITECTURE 9.6).
 */
const SHELL_SPECIAL = /[$`|&;<>()*?[\]{}~#!\\\n\r]/;

/** Quotes a string as one argument for a POSIX shell. */
export function shellQuote(value: string): string {
  return `'${value.replaceAll("'", `'\\''`)}'`;
}

/** Whether the quotes of a command pair up, as the shell reads them. */
function quotesBalanced(command: string): boolean {
  let quote: string | null = null;
  for (const char of command) {
    if (quote) {
      if (char === quote) quote = null;
    } else if (char === "'" || char === '"') {
      quote = char;
    }
  }
  return quote === null;
}

/**
 * The `koroku run` line that runs a command in a service of the app on the
 * host, to paste into an SSH session. A plain command is appended as typed:
 * the host shell removes its quotes and the container gets the same
 * arguments. One the host shell would interpret is wrapped in `sh -c`, so
 * that it runs in the container as typed. Without a command it opens sh.
 */
export function consoleCommand(
  slug: string,
  service: string,
  command: string,
): string {
  const base = `sudo koroku run ${slug} ${service}`;
  const typed = command.trim();
  if (!typed) return base;
  if (SHELL_SPECIAL.test(typed) || !quotesBalanced(typed)) {
    return `${base} sh -c ${shellQuote(typed)}`;
  }
  return `${base} ${typed}`;
}
