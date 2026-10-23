const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.11-417-1da102e152604a35/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
