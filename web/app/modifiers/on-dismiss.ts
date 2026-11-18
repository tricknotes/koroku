import { modifier } from 'ember-modifier';

export type DismissReason = 'outside' | 'escape';

/**
 * Calls back when the reader leaves a popup: a press anywhere outside the
 * element (which should contain both the popup and its trigger), or Escape.
 * The owner closes the popup and decides where focus goes.
 */
export default modifier<{
  Element: HTMLElement;
  Args: { Positional: [dismiss: (reason: DismissReason) => unknown] };
}>((element, [dismiss]) => {
  // `mousedown` rather than `pointerdown`: it also follows a tap, and it is
  // what the test helpers' `click` dispatches.
  const onMouseDown = (event: MouseEvent) => {
    if (event.target instanceof Node && !element.contains(event.target)) {
      void dismiss('outside');
    }
  };
  const onKeyDown = (event: KeyboardEvent) => {
    if (event.key === 'Escape') void dismiss('escape');
  };
  document.addEventListener('mousedown', onMouseDown);
  document.addEventListener('keydown', onKeyDown);
  return () => {
    document.removeEventListener('mousedown', onMouseDown);
    document.removeEventListener('keydown', onKeyDown);
  };
});
