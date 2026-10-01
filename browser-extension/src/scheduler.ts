/** Bounded FIFO lanes. Busy lanes wait without consuming execution slots. */
export class LaneScheduler {
  private lanes = new Map<string, Job[]>();
  private running = new Map<string, Job>();
  private jobs = new Map<string, Job>();
  private ready: string[] = [];
  private active = 0;
  constructor(private readonly concurrency = 4, private readonly capacity = 128) {}

  submit(id: string, lane: string, deadline: number, execute: (signal: AbortSignal) => Promise<unknown>): Promise<unknown> {
    if (this.jobs.has(id)) return Promise.reject(new Error('duplicate command ID'));
    if (this.jobs.size >= this.capacity) return Promise.reject(new Error('browser command capacity reached'));
    if (deadline <= Date.now()) return Promise.reject(new Error('browser command deadline expired before execution'));
    return new Promise((resolve, reject) => {
      const controller = new AbortController();
      const job: Job = { id, lane, execute, controller, resolve, reject,
        timer: setTimeout(() => this.cancel(id), Math.max(1, deadline - Date.now())) };
      this.jobs.set(id, job);
      let queue = this.lanes.get(lane);
      if (!queue) this.lanes.set(lane, queue = []);
      queue.push(job);
      if (!this.running.has(lane) && !this.ready.includes(lane)) this.ready.push(lane);
      this.pump();
    });
  }

  cancel(id: string) {
    const job = this.jobs.get(id);
    if (!job) return false;
    job.controller.abort(new Error('browser command canceled or deadline expired'));
    if (this.running.get(job.lane) === job) return true;
    const queue = this.lanes.get(job.lane);
    if (queue) this.lanes.set(job.lane, queue.filter(item => item !== job));
    this.finish(job, undefined, job.controller.signal.reason);
    this.pump();
    return true;
  }

  cancelAll() { for (const id of [...this.jobs.keys()]) this.cancel(id); }

  private pump() {
    while (this.active < this.concurrency && this.ready.length) {
      const lane = this.ready.shift()!;
      if (this.running.has(lane)) continue;
      const queue = this.lanes.get(lane);
      const job = queue?.shift();
      if (!job) { this.lanes.delete(lane); continue; }
      this.running.set(lane, job); this.active++;
      Promise.resolve().then(() => {
        job.controller.signal.throwIfAborted();
        return job.execute(job.controller.signal);
      }).then(value => this.finish(job, value), error => this.finish(job, undefined, error));
    }
  }

  private finish(job: Job, value?: unknown, error?: unknown) {
    if (!this.jobs.delete(job.id)) return;
    clearTimeout(job.timer);
    if (this.running.get(job.lane) === job) { this.running.delete(job.lane); this.active--; }
    if (this.lanes.get(job.lane)?.length) {
      if (!this.ready.includes(job.lane)) this.ready.push(job.lane);
    } else this.lanes.delete(job.lane);
    if (error !== undefined) job.reject(error); else job.resolve(value);
    this.pump();
  }
}

type Job = {
  id: string; lane: string;
  execute: (signal: AbortSignal) => Promise<unknown>;
  controller: AbortController;
  resolve: (value: unknown) => void;
  reject: (reason: unknown) => void;
  timer: ReturnType<typeof setTimeout>;
};
