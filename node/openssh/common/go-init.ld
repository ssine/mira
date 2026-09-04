/* Keep the delayed constructor pointers read-only after relocation, just as
 * the original .init_array was. GNU ELF linker script, not a portable ABI. */
SECTIONS {
  mira_go_init : ALIGN(8) {
    __start_mira_go_init = .;
    KEEP(*(mira_go_init))
    __stop_mira_go_init = .;
  }
}
INSERT AFTER .init_array;
