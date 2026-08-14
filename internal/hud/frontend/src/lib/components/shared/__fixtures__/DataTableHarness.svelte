<script lang="ts">
  /**
   * Test-only host for DataTable.
   *
   * DataTable's `row` prop is a required Snippet, and a snippet is a compiled
   * construct that cannot be authored from a .ts test file — so the keyboard
   * tests mount this instead, which supplies a minimal one.
   */
  import DataTable from '../DataTable.svelte';

  interface Row {
    id: string;
    name: string;
  }

  let {
    rows = [],
    loading = false,
    keyboardNav = true,
    onRowClick,
  }: {
    rows?: Row[];
    loading?: boolean;
    keyboardNav?: boolean;
    onRowClick?: (row: Row) => void;
  } = $props();

  const columns = [{ key: 'name', label: 'Name' }];
</script>

<DataTable {columns} {rows} {loading} {keyboardNav} {onRowClick} idKey="id" rowLabel="row">
  {#snippet row({ row: r }: { row: Row })}
    <td class="cell-name">{r.name}</td>
  {/snippet}
</DataTable>
