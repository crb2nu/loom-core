// Count of focus-trapped surfaces (dialogs, drawers, the command palette)
// currently mounted. App.svelte's window-level letter shortcuts read it so a
// keypress can't renavigate the page behind an open modal. Maintained by the
// focusTrap action, which runs exactly once per open surface.
class DialogStore {
  openCount = $state(0);

  push() {
    this.openCount++;
  }

  pop() {
    this.openCount = Math.max(0, this.openCount - 1);
  }
}

export const dialogStore = new DialogStore();
