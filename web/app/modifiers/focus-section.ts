import { modifier } from 'ember-modifier';

/**
 * Brings a section of a tab to the top of the page and moves the focus to
 * it, when its argument becomes true: the section named by the `section`
 * query parameter (ARCHITECTURE 9.1). The element needs `tabindex="-1"` and
 * an accessible name (its heading). It runs once the element exists, and
 * again only when the argument changes, so polling does not move the page.
 */
export default modifier<{
  Element: HTMLElement;
  Args: { Positional: [when: boolean] };
}>((element, [when]) => {
  if (!when) return;
  element.scrollIntoView({ block: 'start' });
  element.focus({ preventScroll: true });
});
