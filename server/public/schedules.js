const states = {ready:"等待计划时间",paused:"已暂停",offline:"等待节点上线",queued:"等待执行",dispatching:"正在启动",running:"执行中",succeeded:"成功",failed:"失败",uncertain:"结果待确认",acknowledged:"已确认停止"};
const el = (tag, text, className) => { const node = document.createElement(tag); if (text !== undefined) node.textContent = text; if (className) node.className = className; return node; };
const date = value => value ? new Date(value).toLocaleString() : "—";
export class SchedulesPanel {
  constructor(root, api, toast) {
    this.root=root; this.api=api; this.toast=toast; this.tasks=new Map(); this.cursor=""; this.editID=""; this.createID=crypto.randomUUID();
    this.form=root.querySelector("form");
    this.form.addEventListener("submit",event=>{event.preventDefault();void this.save().catch(e=>toast(e.message));});
    root.addEventListener("click",event=>{const button=event.target.closest("button[data-schedule-action]"); if(button) void this.action(button).catch(e=>toast(e.message));});
  }
  async refresh(nodes) {
    this.names=new Map(nodes.map(n=>[n.nodeId,n.aliases?.[0] || n.displayName || n.nodeKey]));
    const select=this.form.elements.nodeId, selected=select.value; select.replaceChildren();
    for(const node of nodes.filter(n=>n.approvalStatus==="approved" && n.capabilities?.processes)) {const option=el("option",this.names.get(node.nodeId));option.value=node.nodeId;select.append(option);}
    if([...select.options].some(o=>o.value===selected)) select.value=selected;
    await this.load(true);
  }
  async load(reset) {
    const result=await this.api(`/v1/schedules?limit=50&after=${encodeURIComponent(reset?"":this.cursor)}`);
    if(reset)this.tasks.clear();for(const task of result.data || [])this.tasks.set(task.taskId,task);
    this.cursor=result.nextCursor || "";this.root.querySelector('[data-schedule-action="more"]').classList.toggle("hidden",!result.hasMore);
    const list=this.root.querySelector("[data-schedule-list]");list.replaceChildren();
    for(const task of this.tasks.values()) {
      const row=el("tr");
      for(const text of [task.name,this.names?.get(task.nodeId)||task.nodeId,`${task.cron} · ${task.timezone}`,task.enabled?date(task.nextRunAt):"已暂停",`${states[task.state]||task.state}${task.lastStatus ? ` · 最近：${states[task.lastStatus]||task.lastStatus}` : ""}`])row.append(el("td",text));
      const actions=el("td");actions.className="schedule-actions";
      for(const [action,text] of [["edit","编辑"],["run","立即执行"],["toggle",task.enabled?"暂停":"启用"],["history","记录"],["delete","删除"]]){const b=el("button",text,"secondary");b.type="button";b.dataset.scheduleAction=action;b.dataset.taskId=task.taskId;actions.append(b);}
      row.append(actions);list.append(row);
    }
    if(!this.tasks.size){const row=el("tr"),cell=el("td","尚无定时任务");cell.colSpan=6;row.append(cell);list.append(row);}
    if(this.historyID)await this.history(this.historyID);
  }
  async save() {
    const values=new FormData(this.form);let args;
    try{args=JSON.parse(values.get("args")||"[]");if(!Array.isArray(args)||args.some(v=>typeof v!=="string"))throw Error();}catch{throw Error("参数请填写 JSON 字符串数组，例如 [\"/path/job.py\"]");}
    const body={taskId:this.editID||this.createID,name:values.get("name"),nodeId:values.get("nodeId"),cron:values.get("cron"),timezone:values.get("timezone"),command:values.get("command"),cwd:values.get("cwd"),args,executionContext:values.get("executionContext"),enabled:this.form.elements.enabled.checked};
    if(this.editID)body.expectedRevision=this.editRevision;
    const button=this.form.querySelector('[type="submit"]');button.disabled=true;
    try{await this.api(`/v1/schedules${this.editID?`/${this.editID}`:""}`,{method:this.editID?"PUT":"POST",body:JSON.stringify(body)});this.reset();await this.load(true);this.toast("定时任务已保存");}finally{button.disabled=false;}
  }
  reset(){this.form.reset();this.editID="";this.createID=crypto.randomUUID();this.root.querySelector("[data-schedule-editor]").open=false;}
  async history(id, before="") {
    this.historyID=id;const data=await this.api(`/v1/schedules/${id}/runs?limit=10&before=${encodeURIComponent(before)}`);
    const target=this.root.querySelector("[data-schedule-history]");if(!before)target.replaceChildren(el("h3",`${this.tasks.get(id)?.name||"任务"} · 执行记录`));
    target.querySelector('[data-schedule-action="history-more"]')?.remove();
    for(const run of data.data||[]) {
      const details=el("details");details.append(el("summary",`${date(run.createdAt)} · ${states[run.status]||run.status}${run.exitCode!==null?` · 退出码 ${run.exitCode}`:""}`));
      details.append(el("p",`计划时间：${date(run.scheduledFor)} · 更新：${date(run.updatedAt)} · 执行编号：${run.runId}`,"muted"));
      if(run.detail)details.append(el("p",run.detail));
      if(run.outputTruncated)details.append(el("p","仅保留最近 64 KiB 输出；更早的内容已截断。","muted"));
      details.append(el("pre",run.output||"暂无输出","schedule-output"));
      if(run.status==="uncertain"){const b=el("button","确认已停止，解除阻塞","secondary");b.type="button";b.dataset.scheduleAction="acknowledge";b.dataset.taskId=id;b.dataset.runId=run.runId;details.append(b);}
      target.append(details);
    }
    if(!data.data?.length&&!before)target.append(el("p","尚无执行记录","muted"));
    if(data.hasMore){const b=el("button","更早记录","secondary");b.type="button";b.dataset.scheduleAction="history-more";b.dataset.taskId=id;b.dataset.before=data.nextCursor;target.append(b);}
  }
  async action(button) {
    const action=button.dataset.scheduleAction,id=button.dataset.taskId,task=this.tasks.get(id);
    if(action==="cancel"){this.reset();return;}
    if(action==="refresh"){await this.load(true);return;}
    if(action==="more"){await this.load(false);return;}
    if(action==="history-more"){await this.history(id,button.dataset.before);return;}
    if(!task)return;
    if(action==="edit"){
      this.editID=id;this.editRevision=task.revision;
      for(const key of ["name","nodeId","cron","timezone","command","cwd","executionContext"])this.form.elements[key].value=task[key]||"";
      this.form.elements.args.value=JSON.stringify(task.args||[]);this.form.elements.enabled.checked=task.enabled;
      this.root.querySelector("[data-schedule-editor]").open=true;this.form.elements.name.focus();return;
    }
    if(action==="history"){await this.history(id);return;}
    if(action==="delete"&&!confirm(`删除任务“${task.name}”？历史记录仍会保留。`))return;
    if(action==="acknowledge"&&!confirm("请先检查目标节点，确认上次脚本及它触发的后台工作均已停止。解除阻塞后会允许新的执行。已确认停止？"))return;
    button.disabled=true;
    try{
      if(action==="run")await this.api(`/v1/schedules/${id}/run`,{method:"POST",body:JSON.stringify({requestId:crypto.randomUUID()})});
      else if(action==="acknowledge")await this.api(`/v1/schedules/${id}/acknowledge`,{method:"POST",body:JSON.stringify({runId:button.dataset.runId,confirmStopped:true})});
      else await this.api(`/v1/schedules/${id}`,{method:action==="delete"?"DELETE":"PATCH",body:JSON.stringify({expectedRevision:task.revision,enabled:!task.enabled})});
      await this.load(true);if(action==="run")await this.history(id);
    }finally{button.disabled=false;}
  }
}
