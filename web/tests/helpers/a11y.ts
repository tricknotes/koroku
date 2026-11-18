// Checks on the rendered page that a test can pin down without a screen
// reader: every control has a name, references between elements resolve,
// decorative graphics are hidden and a state is never an icon or a colour
// alone. They are deliberately simple approximations of the accessible-name
// rules: enough to notice an icon-only button without a label.

const CONTROLS = [
  'a[href]',
  'button',
  'input:not([type="hidden"])',
  'select',
  'textarea',
  'dialog',
  'nav',
  '[role="switch"]',
  '[role="listbox"]',
  '[role="region"]',
  '[role="meter"]',
  '[tabindex="0"]',
].join(', ');

/** Elements whose own text is their name; the others need a label. */
const NAMED_BY_CONTENT = 'a, button, [role="switch"]';

const REFERENCES = ['aria-labelledby', 'aria-describedby'];

function page(): Element {
  const root = document.querySelector('#ember-testing');
  if (!root) throw new Error('the application is not rendered');
  return root;
}

/** Text an element contributes to a name: not what is hidden from AT. */
function spokenText(element: Element): string {
  let text = '';
  for (const node of element.childNodes) {
    if (node.nodeType === Node.TEXT_NODE) {
      text += node.textContent ?? '';
    } else if (node instanceof Element) {
      if (node.getAttribute('aria-hidden') === 'true') continue;
      if (node instanceof HTMLImageElement) text += node.alt;
      else text += spokenText(node);
    }
  }
  return text.replace(/\s+/g, ' ').trim();
}

function referenced(element: Element, attribute: string): Element[] {
  const ids = (element.getAttribute(attribute) ?? '').split(/\s+/);
  return ids
    .filter(Boolean)
    .map((id) => document.getElementById(id))
    .filter((target): target is HTMLElement => target !== null);
}

/**
 * A rough accessible name: labelledby, aria-label, the `<label>` of a form
 * control or the text of a button or link, then the title.
 */
export function accessibleName(element: Element): string {
  const labelledBy = referenced(element, 'aria-labelledby')
    .map(spokenText)
    .join(' ')
    .trim();
  if (labelledBy) return labelledBy;

  const label = element.getAttribute('aria-label')?.trim();
  if (label) return label;

  if (
    element instanceof HTMLInputElement ||
    element instanceof HTMLSelectElement ||
    element instanceof HTMLTextAreaElement
  ) {
    const labels = [...(element.labels ?? [])].map(spokenText).join(' ').trim();
    if (labels) return labels;
  } else if (element.matches(NAMED_BY_CONTENT)) {
    const text = spokenText(element);
    if (text) return text;
  }
  return element.getAttribute('title')?.trim() ?? '';
}

function describe(element: Element): string {
  const html = element.outerHTML.replace(/\s+/g, ' ');
  return html.length > 160 ? `${html.slice(0, 160)}…` : html;
}

/** Controls and landmarks that a screen reader could not name. */
export function unnamedControls(): string[] {
  return [...page().querySelectorAll(CONTROLS)]
    .filter((element) => accessibleName(element) === '')
    .map((element) => `no accessible name: ${describe(element)}`);
}

/** `aria-labelledby`, `aria-describedby` and `for` pointing at nothing. */
export function brokenReferences(): string[] {
  const problems: string[] = [];
  for (const attribute of REFERENCES) {
    for (const element of page().querySelectorAll(`[${attribute}]`)) {
      const ids = (element.getAttribute(attribute) ?? '')
        .split(/\s+/)
        .filter(Boolean);
      for (const id of ids) {
        if (!document.getElementById(id)) {
          problems.push(
            `${attribute}="${id}" has no target: ${describe(element)}`,
          );
        }
      }
    }
  }
  for (const label of page().querySelectorAll('label[for]')) {
    const id = label.getAttribute('for') ?? '';
    if (!document.getElementById(id)) {
      problems.push(`label for="${id}" has no control: ${describe(label)}`);
    }
  }
  return problems;
}

/** Ids used more than once: labels and descriptions would mix up. */
export function duplicateIds(): string[] {
  const seen = new Set<string>();
  const duplicates = new Set<string>();
  for (const element of page().querySelectorAll('[id]')) {
    if (seen.has(element.id)) duplicates.add(element.id);
    seen.add(element.id);
  }
  return [...duplicates].map((id) => `id "${id}" is used more than once`);
}

/** Icons that are neither hidden from assistive technology nor named. */
export function unlabelledGraphics(): string[] {
  const problems: string[] = [];
  for (const svg of page().querySelectorAll('svg')) {
    if (svg.getAttribute('aria-hidden') === 'true') continue;
    if (svg.getAttribute('aria-label') || svg.querySelector('title')) continue;
    problems.push(`graphic without a name: ${describe(svg)}`);
  }
  for (const image of page().querySelectorAll('img:not([alt])')) {
    problems.push(`image without alt: ${describe(image)}`);
  }
  return problems;
}

/** State indicators that lack their icon or their words. */
export function bareStatuses(): string[] {
  return [...page().querySelectorAll('.status[data-status]')]
    .filter(
      (status) => !status.querySelector('svg') || spokenText(status) === '',
    )
    .map((status) => `state without icon and label: ${describe(status)}`);
}

/** Everything above, for one assertion per screen. */
export function accessibilityProblems(): string[] {
  return [
    ...unnamedControls(),
    ...brokenReferences(),
    ...duplicateIds(),
    ...unlabelledGraphics(),
    ...bareStatuses(),
  ];
}
