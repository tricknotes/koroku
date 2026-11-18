import { module, test } from 'qunit';

type Scheme = 'light' | 'dark';
type Rgb = [number, number, number];

/** The colour a token resolves to in a theme, read back from the browser. */
function resolve(token: string, scheme: Scheme): Rgb {
  const probe = document.createElement('div');
  probe.style.colorScheme = scheme;
  probe.style.color = `var(${token})`;
  document.body.append(probe);
  const color = getComputedStyle(probe).color;
  probe.remove();

  const channels = color.match(/[\d.]+/g)?.map(Number) ?? [];
  if (!color.startsWith('rgb') || channels.length !== 3) {
    throw new Error(`${token} is not an opaque sRGB colour: "${color}"`);
  }
  return channels as Rgb;
}

function luminance(rgb: Rgb): number {
  const [r, g, b] = rgb.map((value) => {
    const channel = value / 255;
    return channel <= 0.04045
      ? channel / 12.92
      : ((channel + 0.055) / 1.055) ** 2.4;
  }) as Rgb;
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

/** WCAG 2 contrast ratio. */
function contrast(a: Rgb, b: Rgb): number {
  const [lighter, darker] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return ((lighter as number) + 0.05) / ((darker as number) + 0.05);
}

const SURFACES = ['--color-bg', '--color-bg-subtle', '--color-bg-hover'];
const STATES = ['running', 'deploying', 'failed', 'stopped', 'pending'];

/** [text colour, background] pairs that carry text somewhere in the UI. */
const PAIRS: [string, string][] = [
  // Body text, at all three levels, on every surface it sits on.
  ...[
    '--color-text',
    '--color-text-secondary',
    '--color-text-tertiary',
  ].flatMap((text) =>
    SURFACES.map((surface): [string, string] => [text, surface]),
  ),
  // Links.
  ...SURFACES.map((surface): [string, string] => ['--color-accent', surface]),
  // State colours as text and icons, on the page and on their own tint.
  ...STATES.flatMap((state): [string, string][] => [
    [`--color-${state}`, '--color-bg'],
    [`--color-${state}`, '--color-bg-hover'],
    [`--color-${state}`, `--color-${state}-bg`],
    // Banners put body text on the tint.
    ['--color-text', `--color-${state}-bg`],
  ]),
  // Solid fills: primary button, production badge, destructive button.
  ['--color-on-solid', '--color-solid'],
  ['--color-on-solid', '--color-solid-hover'],
  ['--color-on-danger', '--color-danger-solid'],
  ['--color-on-danger', '--color-danger-solid-hover'],
];

const AA = 4.5;

module('Unit | Styles | tokens', function () {
  for (const scheme of ['light', 'dark'] as const) {
    test(`text meets WCAG AA contrast in the ${scheme} theme`, function (assert) {
      for (const [text, background] of PAIRS) {
        const ratio = contrast(
          resolve(text, scheme),
          resolve(background, scheme),
        );
        assert.true(
          ratio >= AA,
          `${text} on ${background}: ${ratio.toFixed(2)}:1`,
        );
      }
    });
  }

  test('the two themes differ, and follow color-scheme', function (assert) {
    assert.notDeepEqual(
      resolve('--color-bg', 'light'),
      resolve('--color-bg', 'dark'),
    );
    assert.true(
      luminance(resolve('--color-bg', 'light')) >
        luminance(resolve('--color-text', 'light')),
      'light: dark text on a light page',
    );
    assert.true(
      luminance(resolve('--color-bg', 'dark')) <
        luminance(resolve('--color-text', 'dark')),
      'dark: light text on a dark page',
    );
  });
});
