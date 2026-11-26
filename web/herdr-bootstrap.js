const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.27.0-548-5c83d83d89a9aaca/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
