function assistantText(message) {
  return (message?.content ?? []).filter(block => block.type === 'text').map(block => block.text).join('');
}

export function outcomeFrom(session, assistant, firstStats, failure, stopped, limitHit) {
  const stats = session.getSessionStats();
  let state = stopped ? 'stopped' : assistant?.stopReason === 'stop' ? 'completed' : assistant?.stopReason === 'length' ? 'partial' : 'failed';
  if (failure && state === 'completed') state = 'partial';
  if (limitHit && state !== 'stopped') state = assistant ? 'partial' : 'failed';
  const delta = key => Math.max(0, stats.tokens[key] - firstStats.tokens[key]);
  return {
    state, text: assistantText(assistant), error: failure || assistant?.errorMessage,
    usage: { input: delta('input'), output: delta('output'), cacheRead: delta('cacheRead'), cacheWrite: delta('cacheWrite'), totalTokens: delta('total'), cost: Math.max(0, stats.cost - firstStats.cost) },
  };
}
