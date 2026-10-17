/* Drives smartmontools' own database still gets wrong, so smartctl (and
   Homestack's disk pages) show their attributes as Unknown. install.sh puts
   these in a marked block of /etc/smart_drivedb.h, which smartctl reads
   before its own database. Remove an entry once smartmontools covers it
   (update-smart-drivedb keeps that current), and send it upstream as a
   ticket at https://www.smartmontools.org */

/* Upstream's "WD Blue / Red / Green SSDs" entry only matches the SA510 1 TB
   ("WD Blue SA510 2.5 1000GB"); these are its attribute names, for every
   SA510 (2.5" and M.2, any size). */
{ "WD Blue SA510 SSDs",
  "WD Blue SA510 .*",
  "", "",
  "-v 165,raw48,Block_Erase_Count "
  "-v 166,raw48,Minimum_PE_Cycles_TLC "
  "-v 167,raw48,Max_Bad_Blocks_per_Die "
  "-v 168,raw48,Maximum_PE_Cycles_TLC "
  "-v 169,raw48,Total_Bad_Blocks "
  "-v 170,raw48,Grown_Bad_Blocks "
  "-v 171,raw48,Program_Fail_Count "
  "-v 172,raw48,Erase_Fail_Count "
  "-v 173,raw48,Average_PE_Cycles_TLC "
  "-v 174,raw48,Unexpected_Power_Loss "
  "-v 230,hex48,Media_Wearout_Indicator "
  "-v 233,raw48,NAND_GB_Written_TLC "
  "-v 234,raw48,NAND_GB_Written_SLC "
  "-v 241,raw48,Host_Writes_GiB "
  "-v 242,raw48,Host_Reads_GiB "
  "-v 244,raw48,Temp_Throttle_Status "
},
