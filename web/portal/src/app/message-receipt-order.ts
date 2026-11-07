export class MessageReceiptOrder {
  private readonly pending = new Map<string, Promise<boolean>>();

  begin(scope: string, id: string): (accepted: boolean) => void {
    const key = `${scope}\u0000${id}`;
    let resolve!: (accepted: boolean) => void;
    const promise = new Promise<boolean>((done) => { resolve = done; });
    this.pending.set(key, promise);
    let settled = false;
    return (accepted) => {
      if (settled) return;
      settled = true;
      if (this.pending.get(key) === promise) this.pending.delete(key);
      resolve(accepted);
    };
  }

  previous(scope: string, id: string): Promise<boolean> | undefined {
    return this.pending.get(`${scope}\u0000${id}`);
  }
}
