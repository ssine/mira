// A result ends one foreground response, while a completed background task may
// already have queued another response. Its task snapshot is empty at that point
// and queued_turn_count counts only user input. The pinned CLI's native idle
// event is the turn-over signal; do not close stdin at an intermediate result.
// Older explicit CLI overrides retain the result/background snapshot fallback.
//
// An accepted steer also keeps input open: one queued during the final answer
// runs as another response after that result. The CLI reports a steer as done
// by listing it on a result or by a terminal command_lifecycle, in either order.
export class TurnLifecycle {
  background = false;
  finished = false;
  steers = new Set();
  stateEvents = false;
  responseEnded = false;

  steer(uuid) {
    if (this.finished) return false;
    this.steers.add(uuid);
    return true;
  }

  observe(event) {
    if (event.type === "system" && event.subtype === "session_state_changed") {
      this.stateEvents = true;
      if (event.state === "running") this.responseEnded = false;
      if (event.state === "idle" && this.responseEnded && !this.steers.size)
        this.finished = true;
    }
    if (event.type === "system" && event.subtype === "background_tasks_changed") {
      this.background = event.tasks.some(task => !task.ambient);
    }
    if (event.type === "command_lifecycle" && ["completed", "cancelled"].includes(event.state)) {
      this.steers.delete(event.command_uuid);
    }
    if (event.type === "result" && !event.parent_tool_use_id) {
      for (const uuid of event.user_message_uuids ?? []) this.steers.delete(uuid);
      this.responseEnded = true;
      if (!this.stateEvents)
        this.finished = event.is_error === true || !this.background && !this.steers.size;
    }
    return this.finished;
  }
}
