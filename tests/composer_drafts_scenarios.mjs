// Browser-native assertions shared by the CI runner and local Camoufox validation.
const a = "00000000-0000-4000-8000-0000000000a1";
const b = "00000000-0000-4000-8000-0000000000b2";
const c = "00000000-0000-4000-8000-0000000000c3";
const $ = selector => document.querySelector(selector);
const assert = (condition, message) => { if (!condition) throw Error(message); };
async function until(predicate, message) {
  const deadline = Date.now() + 10000;
  while (!predicate()) {
    if (Date.now() > deadline) throw Error(message);
    await new Promise(resolve => setTimeout(resolve, 20));
  }
}
const ready = () => until(() => $("#agentView:not(.hidden)") && !$("#conversationInput").disabled && $("#conversationDraftStatus").textContent !== "正在恢复草稿…", "Composer did not restore");
const saved = () => until(() => !$("#conversationDraftStatus").textContent.includes("正在") && !$("#conversationDraftStatus").classList.contains("draft-error"), "Draft did not save");
const edit = text => { $("#conversationInput").value = text; $("#conversationInput").dispatchEvent(new Event("input", { bubbles: true })); };
const switchTo = async id => { $(`[data-thread-id="${id}"]`).click(); await ready(); assert(new URL(location.href).searchParams.get("thread") === id, "Wrong selected thread"); };
const control = async value => fetch("/__test/control", { method: "POST", body: JSON.stringify(value) });
function attach() {
  const transfer = new DataTransfer();
  transfer.items.add(new File([new Uint8Array([0, 1, 2, 255]), "草稿"], "draft.bin", { type: "application/octet-stream", lastModified: 123456 }));
  $("#conversationFileInput").files = transfer.files;
  $("#conversationFileInput").dispatchEvent(new Event("change", { bubbles: true }));
}
async function readDraft(id) {
  const { ComposerDrafts } = await import("/composer-drafts.js");
  return new ComposerDrafts().read(`personal:thread:${id}`);
}
export async function runDraftPhase(phase) {
  if (phase === 10) {
    await until(() => $("#dashboardView:not(.hidden)"), "Dashboard did not open");
    $("#globalAgent").click();
  }
  await ready();
  if (phase === 1) {
    edit("A 草稿\n  保留空白  "); attach(); await saved();
    await switchTo(b); assert($("#conversationInput").value === "", "A text leaked into B");
    assert(!$("#conversationAttachments").children.length, "A attachment leaked into B");
    edit("B 草稿"); await saved();
    await switchTo(a); assert($("#conversationInput").value === "A 草稿\n  保留空白  ", "A draft was not restored");
    assert($("#conversationAttachments").textContent.includes("draft.bin"), "A file was not restored");
    // Switch twice without yielding while both reads are outstanding.
    $(`[data-thread-id="${b}"]`).click(); $(`[data-thread-id="${a}"]`).click();
    await ready(); assert($("#conversationInput").value.startsWith("A 草稿"), "Stale read replaced selected draft");
  } else if (phase === 2) {
    assert($("#conversationInput").value === "A 草稿\n  保留空白  ", "Text did not survive reload/reopen");
    const draft = await readDraft(a), file = draft.files[0];
    assert(file.name === "draft.bin" && file.type === "application/octet-stream" && file.lastModified === 123456, "Attachment metadata changed");
    assert(new Uint8Array(await file.arrayBuffer()).join() === "0,1,2,255,232,141,137,231,168,191", "Attachment bytes changed");
    await switchTo(b); assert($("#conversationInput").value === "B 草稿", "B was not persisted");
    $("#agentNewThread").click(); await ready(); assert($("#conversationInput").value === "", "Existing text leaked into new thread");
    edit("新对话草稿"); attach(); await saved();
  } else if (phase === 3) {
    assert($("#conversationInput").value === "新对话草稿", "New-project draft did not survive reopen");
    await control({ fail: true });
    $("#conversationForm").requestSubmit();
    await until(() => $("#conversationNotice").textContent.includes("Fixture send failed") && !$("#conversationSend").disabled, "Expected send failure");
    await saved();
    assert(new URL(location.href).searchParams.get("thread") === c, "Thread was not created");
    const { ComposerDrafts } = await import("/composer-drafts.js");
    const store = new ComposerDrafts();
    const project = await store.read("personal:new-project");
    assert(!await store.read(`personal:new:${project.key}`), "New draft was not moved atomically");
    assert((await readDraft(c)).files.length === 1, "Failed send lost migrated attachment");
  } else if (phase === 4) {
    assert($("#conversationInput").value === "新对话草稿", "Failed send draft was not persistent");
    await control({ delay: 500 });
    $("#conversationForm").requestSubmit();
    await until(() => $("#conversationSend").disabled, "Send did not start");
    edit("发送过程中写的下一条");
    await until(() => !$("#conversationSend").disabled, "Send did not finish"); await saved();
    assert($("#conversationInput").value === "发送过程中写的下一条", "Send cleared newer edits");
    assert(!$("#conversationAttachments").children.length, "Successful send retained attachments");
    assert((await readDraft(c)).text === "发送过程中写的下一条", "New edits were not persisted");
    await control({}); $("#conversationForm").requestSubmit();
    await until(() => !$("#conversationSend").disabled && $("#conversationInput").value === "", "Successful send did not clear text"); await saved();
    assert(!await readDraft(c), "Successful send left a stale draft");
    await switchTo(a); $("#conversationAttachments button").click(); await saved();
    assert(!(await readDraft(a)).files.length, "Attachment removal did not persist");
    edit(""); await saved(); assert(!await readDraft(a), "Empty draft was not deleted");
    await switchTo(b);
  } else if (phase === 5) {
    assert($("#conversationInput").value === "B 草稿", "Other draft was affected by send/clear");
    const { ComposerDrafts } = await import("/composer-drafts.js");
    const original = ComposerDrafts.prototype.write;
    ComposerDrafts.prototype.write = async function(key, value) { this.pending.set(key, value); throw new DOMException("Quota", "QuotaExceededError"); };
    try {
      edit("quota recovery");
      await until(() => $("#conversationDraftStatus").classList.contains("draft-error"), "Storage failure was hidden");
      await switchTo(a); await switchTo(b);
      assert($("#conversationInput").value === "quota recovery", "Storage failure lost in-memory draft");
    } finally { ComposerDrafts.prototype.write = original; }
    edit("recovered after quota"); await saved();
    assert((await readDraft(b)).text === "recovered after quota", "Storage retry did not recover");
  } else if (phase === 6) {
    assert($("#conversationInput").value === "recovered after quota", "Quota recovery was not persistent");
    attach(); await saved();
    await control({ uploadDelay: 500 });
    $("#conversationForm").requestSubmit();
    await until(() => !$("#conversationUploadCancel").classList.contains("hidden"), "Upload did not start");
    $("#conversationUploadCancel").click();
    await until(() => !$("#conversationSend").disabled && $("#conversationNotice").textContent.includes("已取消上传"), "Upload cancellation did not finish");
    assert($("#conversationInput").value === "recovered after quota", "Cancellation discarded text");
    assert((await readDraft(b)).files.length === 1, "Cancellation discarded attachment");
    await control({});
  } else if (phase === 7) {
    assert($("#conversationAttachments").textContent.includes("draft.bin"), "Cancelled attachment did not survive reopen");
    $("#conversationMenuToggle").click(); $("#threadDelete").click();
    await until(() => $("#threadDeleteDialog").open, "Delete dialog did not open");
    $("#threadDeleteConfirm").click();
    await until(() => !$("#threadDeleteDialog").open, "Delete did not finish"); await ready();
    assert(!await readDraft(b), "Deleted thread left a draft");
    await switchTo(a); edit("protected draft"); await saved(); await switchTo(c);
    const { ComposerDrafts } = await import("/composer-drafts.js");
    const original = ComposerDrafts.prototype.read;
    ComposerDrafts.prototype.read = function(key) {
      if (key === `personal:thread:${a}`) return Promise.reject(new Error("Fixture read failure"));
      return original.call(this, key);
    };
    try {
      await switchTo(a);
      assert($("#conversationDraftStatus").classList.contains("draft-error"), "Read failure was hidden");
      edit("unsaved after failed read");
      const disk = await original.call(new ComposerDrafts(), `personal:thread:${a}`);
      assert(disk.text === "protected draft", "Failed read allowed overwriting unseen draft");
    } finally { ComposerDrafts.prototype.read = original; }
    await switchTo(c); await switchTo(a);
    assert($("#conversationInput").value === "unsaved after failed read", "Read failure lost new edits in memory");
    edit("recovered after read failure"); await saved();
  } else if (phase === 8) {
    $("#agentNewThread").click(); await ready();
    edit("work project draft"); await saved();
    const openOtherProject = async () => {
      $("#agentNewProject").click();
      await until(() => $("#projectDialog").open, "Project dialog did not open");
      $("#projectPath").value = "/other";
      $("#projectForm").requestSubmit(); await ready();
    };
    await openOtherProject();
    assert($("#conversationInput").value === "", "Project drafts were mixed");
    edit("other project draft"); await saved();
    await switchTo(a); $("#agentNewThread").click(); await ready();
    assert($("#conversationInput").value === "work project draft", "Work draft was lost");
    await openOtherProject();
    assert($("#conversationInput").value === "other project draft", "Other project draft was lost");
  } else if (phase === 9) {
    assert($("#conversationInput").value === "other project draft", "Reopen did not restore the selected new project");
    assert($("#conversationCwd").value === "/other", "Restored new draft has wrong project directory");
  } else if (phase === 10) {
    assert($("#conversationInput").value === "other project draft", "Dashboard entry did not restore draft");
    assert($("#conversationCwd").value === "/other", "Dashboard entry restored wrong project");
  }
  return `phase ${phase} passed`;
}

// Run as a page module so fault injection shares the application's module realm.
const phase = new URL(import.meta.url).searchParams.get("phase");
if (phase) {
  try { document.documentElement.dataset.draftTest = await runDraftPhase(Number(phase)); }
  catch (error) { document.documentElement.dataset.draftTest = `${error.message}\n${error.stack}`; }
}
