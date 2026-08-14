<script lang="ts">
  /**
   * PatternShelf — the Pattern Loom catalog as a shelf of labeled
   * card-chain books beneath the loom. Jacquard card chains were
   * interchangeable programs (and works of art); here each approved
   * pattern is a book, and a book whose pattern has a run weaving
   * RIGHT NOW visibly feeds — its chain animates. Counts are derived
   * attribution (run → backlog → PlanID → slug), never guessed.
   * An empty catalog renders nothing: no placeholder theater.
   */
  import { router } from '../../stores/router.svelte.ts';
  import type { PatternBook } from '../../utils/patternBooks.ts';

  let { books }: { books: PatternBook[] } = $props();

  function openPatterns(): void {
    router.navigate('mills', 'patterns');
  }
</script>

{#if books.length > 0}
  <div class="shelf" aria-label="Pattern books — the loom's program library">
    <span class="shelf-tag">pattern books</span>
    <div class="shelf-row">
      {#each books as book (book.slug)}
        <button
          type="button"
          class="book"
          class:feeding={book.active > 0}
          onclick={openPatterns}
          title={`${book.name} — makes ${book.makes}. ${book.active} weaving now · ${book.merged} bolts · ${book.escalated} sparks recently. Open the Pattern Loom.`}
        >
          <span class="chain" aria-hidden="true">
            {#each [0, 1, 2, 3] as i (i)}
              <span class="card" style="--i: {i}"></span>
            {/each}
          </span>
          <span class="book-name">{book.name}</span>
          <span class="book-counts">
            {#if book.active > 0}<span class="ct ct-hot">{book.active}▸</span>{/if}
            {#if book.merged > 0}<span class="ct ct-ok">{book.merged}✓</span>{/if}
            {#if book.escalated > 0}<span class="ct ct-wr">{book.escalated}✗</span>{/if}
            {#if book.active + book.merged + book.escalated === 0}<span class="ct ct-dim">shelved</span>{/if}
          </span>
        </button>
      {/each}
    </div>
  </div>
{/if}

<style>
  .shelf {
    display: flex;
    align-items: stretch;
    margin-top: var(--space-2);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    background: var(--bg-secondary);
    overflow: hidden;
    flex-shrink: 0;
  }

  .shelf-tag {
    display: flex;
    align-items: center;
    padding: 0 var(--space-3);
    font-size: var(--text-2xs);
    font-family: var(--font-mono);
    letter-spacing: var(--tracking-wide);
    text-transform: uppercase;
    color: var(--info);
    border-right: 1px solid var(--border);
    background: var(--bg-primary);
    white-space: nowrap;
  }

  .shelf-row {
    display: flex;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    overflow-x: auto;
    flex: 1;
  }

  .book {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-1) var(--space-3);
    background: var(--bg-primary);
    border: 1px solid var(--border-subtle);
    border-left: 3px solid var(--border);
    border-radius: var(--radius-md);
    font-family: var(--font-mono);
    font-size: var(--text-2xs);
    color: var(--fg-secondary);
    cursor: pointer;
    white-space: nowrap;
  }
  .book:hover { border-color: var(--fg-muted); color: var(--fg-primary); }
  .book.feeding {
    border-left-color: var(--accent);
    box-shadow: 0 0 12px rgba(var(--accent-rgb), 0.15) inset;
  }

  /* The card chain: four punched cards on a spine. A feeding book's
     chain steps card-by-card — the program is being read. */
  .chain { display: inline-flex; gap: 2px; }
  .card {
    width: 5px;
    height: 12px;
    border: 1px solid rgba(var(--info-rgb), 0.5);
    border-radius: 1px;
    background:
      radial-gradient(circle at 50% 30%, rgba(var(--info-rgb), 0.7) 0 1px, transparent 1.6px),
      radial-gradient(circle at 50% 70%, rgba(var(--info-rgb), 0.7) 0 1px, transparent 1.6px);
  }
  .feeding .card { animation: chain-step 1.2s steps(1) infinite; animation-delay: calc(var(--i) * 0.3s); }

  .book-name { color: var(--fg-primary); }
  .book-counts { display: inline-flex; gap: var(--space-1); }
  .ct-hot { color: var(--accent); }
  .ct-ok { color: var(--success); }
  .ct-wr { color: var(--warning); }
  .ct-dim { color: var(--fg-dim); }

  @keyframes chain-step {
    0%, 100% { border-color: rgba(var(--info-rgb), 0.5); }
    50% { border-color: rgba(var(--accent-rgb), 0.9); }
  }
  @media (prefers-reduced-motion: reduce) {
    .feeding .card { animation: none; }
  }
</style>
