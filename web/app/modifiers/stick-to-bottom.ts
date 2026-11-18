import { modifier } from 'ember-modifier';

const THRESHOLD = 40;

/**
 * Keeps a scrolling log pinned to its end as content arrives, unless the
 * reader scrolled up. The positional argument only exists to re-run the
 * modifier when the content changes.
 */
export default modifier<{
  Element: HTMLElement;
  Args: { Positional: [content: unknown] };
}>((element) => {
  const pinned = element.dataset['pinned'] !== 'false';
  if (pinned) element.scrollTop = element.scrollHeight;

  const onScroll = () => {
    const distance =
      element.scrollHeight - element.scrollTop - element.clientHeight;
    element.dataset['pinned'] = String(distance <= THRESHOLD);
  };
  element.addEventListener('scroll', onScroll, { passive: true });
  return () => element.removeEventListener('scroll', onScroll);
});
