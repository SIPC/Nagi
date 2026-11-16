// asciinema-player 没有自带类型，只声明用到的部分。
declare module "asciinema-player" {
  export type Player = {
    play(): Promise<void>;
    pause(): void;
    seek(t: number | string): Promise<void>;
    getCurrentTime(): number;
    getDuration(): number;
    dispose(): void;
  };
  export function create(src: string | { data: string }, el: HTMLElement, opts?: Record<string, unknown>): Player;
}
