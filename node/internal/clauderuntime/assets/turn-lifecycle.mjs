// A result ends one foreground response, while native background work may still
// wake the parent for another response. Keep streaming input open until a result
// arrives with no non-ambient background work. Task bookends have no guaranteed
// ordering relative to the authoritative background_tasks_changed snapshot.
export class TurnLifecycle {
  background = false;
  finished = false;

  observe(event) {
    if (event.type === "system" && event.subtype === "background_tasks_changed") {
      this.background = event.tasks.some(task => !task.ambient);
    }
    if (event.type === "result" && !event.parent_tool_use_id) {
      this.finished = event.is_error === true || !this.background;
    }
    return this.finished;
  }
}
