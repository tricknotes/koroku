// Reading and writing environment variables in `.env` format, for the bulk
// edit mode of the environment tab.
//
// The accepted syntax is the common subset of dotenv files:
//
//   # a comment
//   KEY=value            unquoted: trimmed, ` # ...` starts a comment
//   export KEY=value     the `export` prefix is ignored
//   KEY="two\nlines"     double quotes: \n \r \t \" \\ are escapes
//   KEY='as $is'         single quotes: taken literally
//
// Quoted values may span lines. Nothing is expanded (`$VAR` stays as it is).
//
// Problems are reported by line number only. A message never repeats what
// was typed, because any part of a line may be a secret.

import { isValidEnvKey } from './format';
import type { EnvVar } from './types';

export interface DotenvIssue {
  /** 1-based line the problem is on. */
  line: number;
  message: string;
}

export interface DotenvResult {
  /** The variables that could be read, in the order they were written. */
  vars: EnvVar[];
  issues: DotenvIssue[];
}

const ESCAPES: Record<string, string> = {
  n: '\n',
  r: '\r',
  t: '\t',
  '"': '"',
  '\\': '\\',
};

/** Only blanks or a comment may follow a closing quote. */
const REST_OF_LINE = /^\s*(#.*)?$/;

interface Quoted {
  value: string;
  /** Index of the line the closing quote is on. */
  line: number;
  /** What follows the closing quote on that line. */
  rest: string;
}

/**
 * Reads a quoted value that starts right after the opening quote, possibly
 * continuing on the following lines. `null` when the quote is never closed.
 */
function readQuoted(
  lines: string[],
  start: number,
  first: string,
  quote: '"' | "'",
): Quoted | null {
  let value = '';
  let text = first;
  for (let line = start; ;) {
    for (let i = 0; i < text.length; i++) {
      const char = text[i] as string;
      if (char === quote) {
        return { value, line, rest: text.slice(i + 1) };
      }
      const next = text[i + 1];
      if (quote === '"' && char === '\\' && next !== undefined) {
        // An unknown escape keeps its backslash.
        value += ESCAPES[next] ?? `\\${next}`;
        i++;
      } else {
        value += char;
      }
    }
    line++;
    if (line >= lines.length) return null;
    value += '\n';
    text = lines[line] as string;
  }
}

export function parseDotenv(text: string): DotenvResult {
  const lines = text.replace(/\r\n?/g, '\n').split('\n');
  const vars: EnvVar[] = [];
  const issues: DotenvIssue[] = [];
  /** Key → the line it was first defined on. */
  const seen = new Map<string, number>();

  for (let index = 0; index < lines.length; index++) {
    const number = index + 1;
    // The end of the line is kept: it may be inside a quoted value.
    const line = (lines[index] as string).trimStart();
    if (line.trim() === '' || line.startsWith('#')) continue;

    const body = line.replace(/^export\s+/, '');
    const equals = body.indexOf('=');
    if (equals < 0) {
      issues.push({
        line: number,
        message: 'KEY=VALUE の形式ではありません。',
      });
      continue;
    }

    const key = body.slice(0, equals).trim();
    const raw = body.slice(equals + 1).trimStart();
    let value: string;
    let problem = '';

    const quote = raw[0];
    if (quote === '"' || quote === "'") {
      const quoted = readQuoted(lines, index, raw.slice(1), quote);
      if (!quoted) {
        issues.push({
          line: number,
          message: '引用符が閉じられていません。',
        });
        break;
      }
      value = quoted.value;
      index = quoted.line;
      if (!REST_OF_LINE.test(quoted.rest)) {
        problem = '閉じる引用符のあとに余分な文字があります。';
      }
    } else {
      // ` #` starts a comment; a `#` inside a word belongs to the value.
      value = raw.replace(/\s+#.*$/, '').trimEnd();
    }

    if (key === '') {
      problem = 'キーがありません。';
    } else if (!isValidEnvKey(key)) {
      problem =
        'キーには英字かアンダースコアで始まる英数字とアンダースコアだけが使えます。';
    } else if (seen.has(key)) {
      problem = `キーが重複しています（${seen.get(key)} 行目と同じ）。`;
    }

    if (problem) {
      issues.push({ line: number, message: problem });
      continue;
    }
    seen.set(key, number);
    vars.push({ key, value });
  }

  return { vars, issues };
}

/** Values made only of these characters need no quotes. */
const PLAIN = /^[^\s"'#\\]*$/;

function formatValue(value: string): string {
  if (PLAIN.test(value)) return value;
  const escaped = value
    .replace(/\\/g, '\\\\')
    .replace(/"/g, '\\"')
    .replace(/\n/g, '\\n')
    .replace(/\r/g, '\\r')
    .replace(/\t/g, '\\t');
  return `"${escaped}"`;
}

/** One `KEY=value` line per variable; `parseDotenv` reads it back exactly. */
export function formatDotenv(vars: EnvVar[]): string {
  return vars
    .map(({ key, value }) => `${key}=${formatValue(value)}\n`)
    .join('');
}
