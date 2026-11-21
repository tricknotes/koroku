import { modifier } from 'ember-modifier';

/**
 * Scrolls the page so that the element shows at the top, when its argument
 * becomes true: a deployment opened from a link (the overview's recent
 * deployments, GitHub's deployments). It runs once the element exists, and
 * again only when the argument changes, so polling does not scroll again.
 */
export default modifier<{
  Element: HTMLElement;
  Args: { Positional: [when: boolean] };
}>((element, [when]) => {
  if (when) element.scrollIntoView({ block: 'start' });
});
