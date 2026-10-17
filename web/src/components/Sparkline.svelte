<script>
  // A small inline chart for the dashboard tiles: one or two series, newest
  // on the right, scaled to `max` (or the largest value shown).
  let { series = [], max = 0, height = 34, label = '' } = $props(); // series: [{ values, color }]

  const W = 100;
  const top = $derived(Math.max(max, ...series.flatMap((s) => s.values), 1e-9));
  function points(values) {
    const n = values.length;
    if (n < 2) return '';
    return values.map((v, i) => `${((i / (n - 1)) * W).toFixed(2)},${(height - 1 - (Math.min(v, top) / top) * (height - 3)).toFixed(2)}`).join(' ');
  }
</script>

<svg class="spark" viewBox="0 0 {W} {height}" preserveAspectRatio="none" style="height:{height}px" role="img" aria-label={label}>
  {#each series as s, i}
    {@const pts = points(s.values)}
    {#if pts}
      {#if i === 0}<polygon points="0,{height} {pts} {W},{height}" fill={s.color} opacity="0.13" />{/if}
      <polyline points={pts} fill="none" stroke={s.color} stroke-width="1.6" vector-effect="non-scaling-stroke" stroke-linejoin="round" stroke-linecap="round" />
    {/if}
  {/each}
</svg>

<style>
  .spark { display: block; width: 100%; overflow: visible; }
</style>
