import { modifier } from 'ember-modifier';

/**
 * Keeps the active link of a horizontally scrolling navigation in view (the
 * app tabs on a narrow screen). It scrolls the navigation only, never the
 * page. The positional argument only exists to re-run the modifier when the
 * current route changes.
 */
export default modifier<{
  Element: HTMLElement;
  Args: { Positional: [current: unknown] };
}>((element, [current]) => {
  // Arguments are tracked only once they are read.
  void current;
  const active = element.querySelector<HTMLElement>('.active');
  if (!active) return;
  const frame = element.getBoundingClientRect();
  const link = active.getBoundingClientRect();
  if (link.left < frame.left) {
    element.scrollLeft -= frame.left - link.left;
  } else if (link.right > frame.right) {
    element.scrollLeft += link.right - frame.right;
  }
});
