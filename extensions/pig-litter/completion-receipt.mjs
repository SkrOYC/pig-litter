export function queueCompletion(run, message, appendRetained) {
  const session = run.session;
  return new Promise((resolve, reject) => {
    let settled = false;
    let unsubscribe;
    const clear = () => {
      if (settled) return false;
      settled = true;
      unsubscribe?.();
      run.completionReceipts.delete(retire);
      return true;
    };
    const retire = () => {
      if (!clear()) return;
      try { appendRetained(); resolve(); }
      catch (error) { reject(error); }
    };
    unsubscribe = session.subscribe(event => {
      const received = event.message;
      if (event.type !== 'message_end' || received?.role !== 'custom' || received.customType !== message.customType) return;
      if (received.details?.childId !== message.details.childId || received.details?.generation !== message.details.generation) return;
      if (clear()) resolve();
    });
    run.completionReceipts.add(retire);
    Promise.resolve().then(() => {
      if (!settled) return session.sendCustomMessage(message, { triggerTurn: false });
    }).catch(error => { run.completionSendError = String(error instanceof Error ? error.message : error).slice(0, 256); });
  });
}

export function retireCompletions(run) {
  for (const retire of [...run.completionReceipts]) retire();
}
