import { modifier } from 'ember-modifier';

/** How close to the end still counts as "at the end" (about one row). */
const THRESHOLD = 24;

/**
 * For a scrolling log: keeps the end in view while `follow` is on, and
 * reports whether the reader is at the end whenever they scroll, so the
 * owner can stop following when they scroll up and resume when they come
 * back. The positional arguments only exist to re-run the modifier when
 * what is rendered changes (the rows, the wrapping).
 */
export default modifier<{
  Element: HTMLElement;
  Args: {
    Positional: unknown[];
    Named: { follow: boolean; onScroll: (atEnd: boolean) => void };
  };
}>((element, content, { follow, onScroll }) => {
  // Reading the arguments is what subscribes the modifier to them.
  content.forEach(() => undefined);
  if (follow) element.scrollTop = element.scrollHeight;

  const listener = () => {
    const distance =
      element.scrollHeight - element.scrollTop - element.clientHeight;
    onScroll(distance <= THRESHOLD);
  };
  element.addEventListener('scroll', listener, { passive: true });
  return () => element.removeEventListener('scroll', listener);
});
