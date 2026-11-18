import { modifier } from 'ember-modifier';

/** How close to the end still counts as "at the end", in pixels. */
const THRESHOLD = 24;

/**
 * Keeps a scrolling log at its end while `following`, and reports when the
 * reader leaves the end (scrolls up) or comes back to it, so the owner can
 * stop and resume following. The positional argument only exists to re-run
 * the modifier when the content changes (pass something that changes with
 * it, such as the number of lines).
 */
export default modifier<{
  Element: HTMLElement;
  Args: {
    Positional: [content: unknown];
    Named: { following: boolean; onChange: (following: boolean) => unknown };
  };
}>((element, [content], { following, onChange }) => {
  // Arguments are tracked only once they are read.
  void content;
  if (following) element.scrollTop = element.scrollHeight;

  const onScroll = () => {
    const distance =
      element.scrollHeight - element.scrollTop - element.clientHeight;
    const atEnd = distance <= THRESHOLD;
    if (atEnd !== following) void onChange(atEnd);
  };
  element.addEventListener('scroll', onScroll, { passive: true });
  return () => element.removeEventListener('scroll', onScroll);
});
