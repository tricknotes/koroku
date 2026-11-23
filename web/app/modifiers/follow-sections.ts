import { modifier } from 'ember-modifier';

/** How far below the top of the window a section counts as the current one. */
const THRESHOLD = 160;

/**
 * Tells which section of a long page is being read, as the page scrolls:
 * the last element with `data-settings-section` whose top has passed near
 * the top of the window, or the last one once the page is scrolled to its
 * end. The settings index marks it (ARCHITECTURE 9.1).
 */
export default modifier<{
  Element: HTMLElement;
  Args: { Positional: [onChange: (id: string) => void] };
}>((element, [onChange]) => {
  let frame = 0;
  let last = '';

  const update = () => {
    frame = 0;
    const sections = Array.from(
      element.querySelectorAll<HTMLElement>('[data-settings-section]'),
    );
    if (sections.length === 0) return;
    const atEnd =
      window.innerHeight + window.scrollY >=
      document.documentElement.scrollHeight - 2;
    let current = sections[0]!;
    if (atEnd && window.scrollY > 0) {
      current = sections[sections.length - 1]!;
    } else {
      for (const section of sections) {
        if (section.getBoundingClientRect().top <= THRESHOLD) current = section;
      }
    }
    const id = current.dataset['settingsSection'] ?? '';
    if (id && id !== last) {
      last = id;
      onChange(id);
    }
  };

  const schedule = () => {
    if (frame === 0) frame = requestAnimationFrame(update);
  };

  window.addEventListener('scroll', schedule, { passive: true });
  return () => {
    window.removeEventListener('scroll', schedule);
    if (frame !== 0) cancelAnimationFrame(frame);
  };
});
