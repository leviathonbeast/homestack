// Apply the saved theme before the page paints, so there's no flash.
try {
  var t = localStorage.getItem('homestack-theme');
  if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t;
} catch (e) {}
