import type { TOC } from '@ember/component/template-only';

/**
 * How a path is painted: a stroked line (the default), a filled shape, a
 * mark cut out of a filled shape (drawn in the page colour) or the faint
 * track under a spinner.
 */
type Paint = 'line' | 'fill' | 'cut' | 'track';

interface Shape {
  d: string;
  paint?: Paint;
}

const DISC = 'M8 1a7 7 0 1 0 0 14A7 7 0 0 0 8 1z';
/** A third of a circle: the moving part of the spinner. */
const ARC = 'M8 1.75a6.25 6.25 0 0 1 5.41 9.38';
const RING = 'M8 1.75a6.25 6.25 0 1 0 0 12.5 6.25 6.25 0 0 0 0-12.5z';
const CROSS = 'M5.75 5.75l4.5 4.5M10.25 5.75l-4.5 4.5';

/**
 * The whole icon set: small line drawings on a 16px grid, drawn for this
 * project. Keep it short; an icon earns its place only where a label alone
 * would be slower to read.
 */
const ICONS = {
  // The five states. Each has its own silhouette, so they can be told apart
  // without colour.
  'status-running': [
    { d: DISC, paint: 'fill' },
    { d: 'M5 8.3l2 2 4-4.4', paint: 'cut' },
  ],
  'status-deploying': [{ d: RING, paint: 'track' }, { d: ARC }],
  'status-failed': [
    { d: DISC, paint: 'fill' },
    { d: CROSS, paint: 'cut' },
  ],
  'status-stopped': [
    { d: RING },
    { d: 'M5.75 5.75h4.5v4.5h-4.5z', paint: 'fill' },
  ],
  'status-pending': [{ d: RING }, { d: 'M8 4.75V8l2.1 1.4' }],

  check: [{ d: 'M3 8.5l3.25 3.25L13 4.75' }],
  'chevron-down': [{ d: 'M4 6l4 4 4-4' }],
  copy: [
    {
      d: 'M6.75 5.75h5.5a1 1 0 0 1 1 1v5.5a1 1 0 0 1-1 1h-5.5a1 1 0 0 1-1-1v-5.5a1 1 0 0 1 1-1z',
    },
    {
      d: 'M10.25 3.25v-.5a1 1 0 0 0-1-1h-5.5a1 1 0 0 0-1 1v5.5a1 1 0 0 0 1 1h.5',
    },
  ],
  error: [{ d: RING }, { d: CROSS }],
  external: [
    { d: 'M9.5 2.75h3.75V6.5' },
    { d: 'M13.25 2.75L7.75 8.25' },
    {
      d: 'M6.5 4H4.25A1.5 1.5 0 0 0 2.75 5.5v6.25a1.5 1.5 0 0 0 1.5 1.5h6.25a1.5 1.5 0 0 0 1.5-1.5V9.5',
    },
  ],
  eye: [
    { d: 'M1.5 8s2.4-4.5 6.5-4.5S14.5 8 14.5 8s-2.4 4.5-6.5 4.5S1.5 8 1.5 8z' },
    { d: 'M8 6.1a1.9 1.9 0 1 0 0 3.8 1.9 1.9 0 0 0 0-3.8z' },
  ],
  'eye-off': [
    {
      d: 'M6.4 3.7A6.5 6.5 0 0 1 8 3.5c4.1 0 6.5 4.5 6.5 4.5a11 11 0 0 1-1.6 2.1',
    },
    {
      d: 'M10.9 11.7A6.3 6.3 0 0 1 8 12.5C3.9 12.5 1.5 8 1.5 8a10.6 10.6 0 0 1 2.9-3.3',
    },
    { d: 'M6.7 6.6a1.9 1.9 0 0 0 2.7 2.7' },
    { d: 'M2.5 2.5l11 11' },
  ],
  info: [{ d: RING }, { d: 'M8 7.4v3.35' }, { d: 'M8 5.15v.1' }],
  logo: [
    {
      d: 'M3.5 1.5h9a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2h-9a2 2 0 0 1-2-2v-9a2 2 0 0 1 2-2z',
      paint: 'fill',
    },
    { d: 'M5 5.25h6M5 8h6M5 10.75h3.25', paint: 'cut' },
  ],
  moon: [
    { d: 'M13.25 9.6A5.75 5.75 0 0 1 6.4 2.75a5.75 5.75 0 1 0 6.85 6.85z' },
  ],
  spinner: [{ d: RING, paint: 'track' }, { d: ARC }],
  sun: [
    { d: 'M8 5.25a2.75 2.75 0 1 0 0 5.5 2.75 2.75 0 0 0 0-5.5z' },
    {
      d: 'M8 1.25v1.5M8 13.25v1.5M1.25 8h1.5M13.25 8h1.5M3.23 3.23l1.06 1.06M11.71 11.71l1.06 1.06M3.23 12.77l1.06-1.06M11.71 4.29l1.06-1.06',
    },
  ],
  system: [
    {
      d: 'M2.75 2.75h10.5a1 1 0 0 1 1 1v6.5a1 1 0 0 1-1 1H2.75a1 1 0 0 1-1-1v-6.5a1 1 0 0 1 1-1z',
    },
    { d: 'M5.5 13.75h5M8 11.25v2.5' },
  ],
  warning: [
    { d: 'M8 2.25l6.25 11H1.75z' },
    { d: 'M8 6.6v3' },
    { d: 'M8 11.5v.1' },
  ],
} satisfies Record<string, Shape[]>;

export type IconName = keyof typeof ICONS;

const SPINNING = new Set<IconName>(['status-deploying', 'spinner']);

function shapes(name: IconName): Shape[] {
  return ICONS[name];
}

function paintClass(shape: Shape): string | undefined {
  return shape.paint && shape.paint !== 'line'
    ? `icon-${shape.paint}`
    : undefined;
}

function iconClass(name: IconName): string {
  return SPINNING.has(name) ? 'icon icon-spin' : 'icon';
}

/**
 * Inline SVG icon, sized by the font size of its parent (1em). Decorative by
 * default: the text next to it carries the meaning.
 */
const Icon: TOC<{
  Element: SVGSVGElement;
  Args: { name: IconName };
}> = <template>
  <svg
    class={{iconClass @name}}
    viewBox="0 0 16 16"
    aria-hidden="true"
    focusable="false"
    data-icon={{@name}}
    ...attributes
  >
    {{#each (shapes @name) as |shape|}}
      <path d={{shape.d}} class={{paintClass shape}} />
    {{/each}}
  </svg>
</template>;

export default Icon;
