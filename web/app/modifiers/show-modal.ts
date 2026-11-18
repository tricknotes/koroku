import { modifier } from 'ember-modifier';

/**
 * Opens a `<dialog>` as a modal while it is rendered. The browser then
 * traps focus inside it, makes the rest of the page inert and closes it on
 * Escape (which the dialog component turns into "cancel").
 */
export default modifier<{ Element: HTMLDialogElement }>((element) => {
  const previous = document.activeElement;
  element.showModal();
  return () => {
    element.close();
    if (previous instanceof HTMLElement && previous.isConnected) {
      previous.focus();
    }
  };
});
