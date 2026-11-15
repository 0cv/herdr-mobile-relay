const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.25.1-505-a5e60bd220aed8a9/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
