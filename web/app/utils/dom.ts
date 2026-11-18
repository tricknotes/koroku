// Small template helpers for reading form controls.

type Control = HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement;

/** Value of the control an `input` / `change` event came from. */
export function inputValue(event: Event): string {
  return (event.target as Control).value;
}

/** Checked state of the checkbox a `change` event came from. */
export function inputChecked(event: Event): boolean {
  return (event.target as HTMLInputElement).checked;
}

type KeysOfType<T, V> = {
  [K in keyof T]-?: T[K] extends V ? K : never;
}[keyof T];

/**
 * `{{on "input" (setValue this.form "slug")}}`: copies the control's value
 * into a (tracked) string property.
 */
export function setValue<T extends object>(
  target: T,
  key: KeysOfType<T, string>,
): (event: Event) => void {
  return (event) => {
    (target as Record<PropertyKey, unknown>)[key] = inputValue(event);
  };
}

/** Same as `setValue` for a checkbox and a boolean property. */
export function setChecked<T extends object>(
  target: T,
  key: KeysOfType<T, boolean>,
): (event: Event) => void {
  return (event) => {
    (target as Record<PropertyKey, unknown>)[key] = inputChecked(event);
  };
}

/** Wraps a submit handler so the browser does not post the form itself. */
export function preventDefault(handler: () => unknown): (event: Event) => void {
  return (event) => {
    event.preventDefault();
    void handler();
  };
}
