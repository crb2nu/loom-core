<script lang="ts">
  import { millsStore, type BoltCard, type BoltGrade } from '../../stores/mills.svelte.ts';
  import { formatChange, formatScore } from '../../utils/shiftHelpers.ts';
  import { fmtCost } from './shared/format.ts';
  let { cards, onclose, ongraded = () => {} }: { cards: BoltCard[]; onclose: () => void; ongraded?: (card: BoltCard, grade: BoltGrade) => void } = $props();
  let index = $state(0); let busy = $state(false); let error = $state(''); let card = $derived(cards[index]);
  function editable(target: EventTarget | null): boolean { const el=target as HTMLElement|null; return !!el&&(el.isContentEditable||/^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName)); }
  async function grade(value: BoltGrade): Promise<void> { if(!card||busy)return; busy=true; error=''; try { await millsStore.gradeBolt(card,value,card.grade?.note??''); ongraded(card,value); if(index<cards.length-1)index+=1; else onclose(); } catch(e){error=e instanceof Error?e.message:String(e)} finally{busy=false} }
  function keydown(event: KeyboardEvent): void { if(event.metaKey||event.ctrlKey||event.altKey||event.shiftKey||editable(event.target))return; if(event.key==='Escape')onclose(); if(event.key==='ArrowLeft'&&index>0)index-=1; if(event.key==='ArrowRight'&&index<cards.length-1)index+=1; const grades:Record<string,BoltGrade>={'1':'keep','2':'meh','3':'regret'}; if(grades[event.key])void grade(grades[event.key]); }
</script>
<svelte:window onkeydown={keydown}/>
<div class="gw" role="dialog" aria-modal="true" aria-label="Grade the shift">
  <header><strong>grade the shift</strong><span>{cards.length?index+1:0} of {cards.length}</span></header>
  {#if card}<article><h3>{card.title||card.backlog_id}</h3><div class="meta"><span>{card.backlog_id}</span>{#if card.mr.iid}<a href={card.mr.url}>!{card.mr.iid}</a>{/if}<span>{formatChange(card.diff.files,card.diff.added,card.diff.removed)}</span><span>eval {formatScore(card.eval.min_score)}</span><span>{fmtCost(card.cost_usd)}</span></div><div class="grades"><button disabled={busy} onclick={()=>grade('keep')}>1 · keep</button><button disabled={busy} onclick={()=>grade('meh')}>2 · meh</button><button disabled={busy} onclick={()=>grade('regret')}>3 · regret</button></div>{#if error}<p role="alert">{error}</p>{/if}</article>{/if}
  <footer><button disabled={index===0} onclick={()=>index-=1}>← previous</button><button disabled={index>=cards.length-1} onclick={()=>index+=1}>next →</button><button onclick={onclose}>close</button></footer>
</div>
<style>.gw{position:fixed;inset:12% max(5%,calc((100% - 620px)/2));z-index:calc(var(--z-modal) + 3);background:var(--bg-secondary);border:1px solid var(--border);border-radius:var(--radius-lg);padding:var(--space-4);box-shadow:0 24px 48px #0008}header,footer,.meta,.grades{display:flex;gap:var(--space-2);align-items:center;flex-wrap:wrap}header{justify-content:space-between}article{margin:var(--space-5) 0;padding:var(--space-4);border:1px solid var(--border);border-radius:var(--radius-md)}h3{margin:0 0 var(--space-2)}.meta{color:var(--fg-muted);font-size:var(--text-xs)}.grades{margin-top:var(--space-4)}</style>
