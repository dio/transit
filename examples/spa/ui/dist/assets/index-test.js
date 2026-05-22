const root = document.getElementById("root");

const routes = {
  "/": "Home",
  "/about": "About",
  "/dashboard": "Dashboard",
};

function titleForPath(pathname) {
  return routes[pathname] || "Home";
}

function render() {
  const title = titleForPath(window.location.pathname);
  root.innerHTML = "";

  const nav = document.createElement("nav");
  for (const [href, label] of Object.entries(routes)) {
    const link = document.createElement("a");
    link.href = href;
    link.textContent = label;
    nav.appendChild(link);
  }

  const heading = document.createElement("h2");
  heading.textContent = title;

  root.appendChild(nav);
  root.appendChild(heading);

  if (title === "Dashboard") {
    const button = document.createElement("button");
    button.type = "button";
    button.textContent = "Fetch server time";

    const output = document.createElement("pre");
    output.textContent = "No server time loaded";

    button.addEventListener("click", () => {
      fetch("/api/time")
        .then((res) => res.json())
        .then((body) => {
          output.textContent = JSON.stringify({
            filter: "api-backend",
            time: body.time,
          });
        });
    });

    root.appendChild(button);
    root.appendChild(output);
  }
}

render();
