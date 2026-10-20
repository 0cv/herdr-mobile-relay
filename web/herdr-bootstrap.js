const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.10-410-a490065b540d3e79/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
