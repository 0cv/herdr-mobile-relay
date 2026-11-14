const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.25.1-499-ef54df4df101b793/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
