import { modifier } from 'ember-modifier';

/** How long the section is kept in place while the page above it loads. */
const SETTLE_MS = 2000;

/** What the reader does to move the page on their own. */
const READER_EVENTS = ['wheel', 'touchstart', 'keydown', 'pointerdown'];

/**
 * Brings a section of a tab to the top of the page and moves the focus to
 * it, when its argument becomes true: the section named by the `section`
 * query parameter (ARCHITECTURE 9.1). The element needs `tabindex="-1"` and
 * an accessible name (its heading). It runs once the element exists, and
 * again only when the argument changes, so polling does not move the page.
 *
 * Parts above the section may still be loading (the domains in the
 * settings) and push it down; until the reader moves the page, or for a
 * couple of seconds, it is brought back to the top whenever the page grows.
 */
export default modifier<{
  Element: HTMLElement;
  Args: { Positional: [when: boolean] };
}>((element, [when]) => {
  if (!when) return;
  element.scrollIntoView({ block: 'start' });
  element.focus({ preventScroll: true });

  if (typeof ResizeObserver === 'undefined') return;
  const observer = new ResizeObserver(() => {
    element.scrollIntoView({ block: 'start' });
  });
  observer.observe(document.body);
  const stop = () => {
    observer.disconnect();
    clearTimeout(timer);
    for (const name of READER_EVENTS) {
      window.removeEventListener(name, stop, true);
    }
  };
  const timer = setTimeout(stop, SETTLE_MS);
  for (const name of READER_EVENTS) {
    window.addEventListener(name, stop, { capture: true, passive: true });
  }
  return stop;
});
