// A result ends one foreground response, while native background work may still
// wake the parent for another response. Keep streaming input open until a result
// arrives with no non-ambient background work. Task bookends have no guaranteed
// ordering relative to the authoritative background_tasks_changed snapshot.
//
// An accepted steer also keeps input open: one queued during the final answer
// runs as another response after that result. The CLI reports a steer as done
// by listing it on a result or by a terminal command_lifecycle, in either order.
export class TurnLifecycle {
  background = false;
  finished = false;
  steers = new Set();

  steer(uuid) {
    if (this.finished) return false;
    this.steers.add(uuid);
    return true;
  }

  observe(event) {
    if (event.type === "system" && event.subtype === "background_tasks_changed") {
      this.background = event.tasks.some(task => !task.ambient);
    }
    if (event.type === "command_lifecycle" && ["completed", "cancelled"].includes(event.state)) {
      this.steers.delete(event.command_uuid);
    }
    if (event.type === "result" && !event.parent_tool_use_id) {
      for (const uuid of event.user_message_uuids ?? []) this.steers.delete(uuid);
      this.finished = event.is_error === true || !this.background && !this.steers.size;
    }
    return this.finished;
  }
}
