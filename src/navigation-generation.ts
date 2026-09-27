export interface NavigationGeneration {
  readonly current: number;
  readonly beforeNavigation?: number;
}

export function beginNavigationGeneration(state: NavigationGeneration): NavigationGeneration {
  return {
    current: state.current + 1,
    beforeNavigation: state.beforeNavigation ?? state.current,
  };
}

export function commitNavigationGeneration(state: NavigationGeneration): NavigationGeneration {
  return { current: state.current };
}

export function rollbackNavigationGeneration(state: NavigationGeneration): NavigationGeneration {
  return state.beforeNavigation === undefined
    ? state
    : { current: state.beforeNavigation };
}
